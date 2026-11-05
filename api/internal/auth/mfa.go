package auth

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// MFA timing and limits.
const (
	// MFAChallengeTTL is how long the second step of a sign-in may take.
	MFAChallengeTTL = 5 * time.Minute
	// MFAChallengeMaxAttempts invalidates a challenge after this many wrong
	// codes; the per-account throttle limits fresh challenges as well.
	MFAChallengeMaxAttempts = 5
	// MFAEnrolmentTTL bounds how long an unconfirmed secret stays valid.
	MFAEnrolmentTTL = 10 * time.Minute
)

func errMFADisabled() error {
	return fault.New(fault.KindUnavailable, "mfaUnavailable", "Multi-factor authentication is not available.")
}

func errChallenge() error {
	return fault.Unauthenticated("mfaChallengeInvalid", "This sign-in attempt has expired. Sign in again.")
}

// challenge records a pending second-factor step after a correct password.
func (s *Service) challenge(ctx context.Context, ws string, u users.User, in LoginInput) (Issued, error) {
	now := s.clock.Now()
	token, hash := newOpaqueToken()
	if _, err := s.mfa.CreateChallenge(ctx, MFAChallenge{WorkspaceId: ws, UserId: u.Id, TokenHash: hash, CreatedAt: now,
		ExpiresAt: now.Add(MFAChallengeTTL), UserAgent: truncate(in.UserAgent, 256), ClientIP: in.ClientIP}); err != nil {
		return Issued{}, err
	}
	s.appendAudit(ctx, ws, audit.Actor{Type: audit.ActorUser, Id: u.Id}, audit.AuthMFAChallenged, "user/"+u.Id, "success", "")
	return Issued{MFARequired: true, MFAToken: token, MFAExpiresIn: int(MFAChallengeTTL / time.Second), User: u}, nil
}

// appendAudit writes a standalone audit event, logging (not failing) on error.
func (s *Service) appendAudit(ctx context.Context, ws string, actor audit.Actor, action audit.Action, target, outcome, reason string) {
	ev := audit.New(ctx, actor, action, target, s.clock.Now())
	ev.WorkspaceId, ev.Outcome, ev.Reason = ws, outcome, reason
	if err := s.audit.Append(ctx, ev); err != nil {
		s.log.Warn("could not append audit event", "action", action, "error", err.Error())
	}
}

// MFAInput completes a two-step sign-in with a TOTP code or a recovery code.
type MFAInput struct {
	Token        string
	Code         string
	RecoveryCode string
	UserAgent    string
	ClientIP     string
}

// CompleteMFA verifies the second factor of a challenged sign-in and opens
// the session. Wrong codes count against the challenge (5 attempts) and
// the account's MFA throttle (5 failures per 15 minutes).
func (s *Service) CompleteMFA(ctx context.Context, in MFAInput) (Issued, error) {
	if s.mfa == nil {
		return Issued{}, errMFADisabled()
	}
	var v validation.Collector
	v.String(validation.AuthOpaqueToken, "mfaToken", in.Token)
	switch {
	case in.Code != "" && in.RecoveryCode != "":
		v.Add("code", validation.CodeInvalidChoice, "Send either a code or a recovery code, not both.")
	case in.Code != "":
		v.String(validation.AuthMFACode, "code", in.Code)
	case in.RecoveryCode != "":
		v.String(validation.AuthRecoveryCode, "recoveryCode", in.RecoveryCode)
	default:
		v.Add("code", validation.CodeRequired, "Enter the code from your authenticator app or a recovery code.")
	}
	if err := v.Err(); err != nil {
		return Issued{}, err
	}
	ws, err := s.workspace(ctx)
	if err != nil {
		return Issued{}, err
	}
	now := s.clock.Now()
	ch, err := s.mfa.ChallengeByHash(ctx, digest(in.Token))
	if errors.Is(err, ErrChallengeNotFound) {
		return Issued{}, errChallenge()
	}
	if err != nil {
		return Issued{}, err
	}
	if ch.WorkspaceId != ws || ch.UsedAt != nil || !now.Before(ch.ExpiresAt) || ch.Attempts >= MFAChallengeMaxAttempts {
		return Issued{}, errChallenge()
	}
	keys := []string{"mfa:user:" + ch.UserId}
	if in.ClientIP != "" {
		keys = append(keys, "login:ip:"+in.ClientIP)
	}
	if wait, err := s.throttle.Check(ctx, keys); err != nil {
		return Issued{}, err
	} else if wait > 0 {
		return Issued{}, fault.RateLimited("mfaThrottled", "Too many incorrect codes. Try again later.", wait)
	}
	u, err := s.users.Get(ctx, ws, ch.UserId)
	if err != nil || !u.Active() || !u.MFAEnabled() {
		if err != nil && !errors.Is(err, users.ErrNotFound) {
			return Issued{}, err
		}
		return Issued{}, errChallenge()
	}
	// Count the attempt on the challenge and the throttle before checking
	// the code, so concurrent guesses cannot exceed either budget.
	if n, err := s.mfa.ChallengeAttempt(ctx, ch.Id); err != nil {
		return Issued{}, err
	} else if n > MFAChallengeMaxAttempts {
		return Issued{}, errChallenge()
	}
	if wait, err := s.throttle.Begin(ctx, keys); err != nil {
		return Issued{}, err
	} else if wait > 0 {
		return Issued{}, fault.RateLimited("mfaThrottled", "Too many incorrect codes. Try again later.", wait)
	}
	method, ok, err := s.verifySecondFactor(ctx, ws, u.Id, in.Code, in.RecoveryCode, now)
	if err != nil {
		return Issued{}, err
	}
	actor := audit.Actor{Type: audit.ActorUser, Id: u.Id}
	if !ok {
		s.appendAudit(ctx, ws, actor, audit.AuthMFA, "user/"+u.Id, "failure", method)
		return Issued{}, fault.Unauthenticated("invalidMfaCode", "The code is incorrect or has already been used.")
	}
	if won, err := s.mfa.ConsumeChallenge(ctx, ch.Id, now); err != nil {
		return Issued{}, err
	} else if !won {
		return Issued{}, errChallenge()
	}
	if err := s.throttle.Reset(ctx, keys[:1]); err != nil {
		s.log.Warn("could not reset mfa throttle", "error", err.Error())
	}
	if err := s.throttle.Forgive(ctx, keys[1:]); err != nil {
		s.log.Warn("could not adjust login throttle", "error", err.Error())
	}
	if method == "recoveryCode" {
		s.appendAudit(ctx, ws, actor, audit.UserMFARecoveryCodeUsed, "user/"+u.Id, "success", "")
	}
	return s.openSession(ctx, ws, u, in.UserAgent, in.ClientIP, method)
}

// verifySecondFactor checks a TOTP code (rejecting replays of an already
// used time step) or consumes a recovery code.
func (s *Service) verifySecondFactor(ctx context.Context, ws, userID, code, recovery string, now time.Time) (string, bool, error) {
	if recovery != "" {
		ok, err := s.mfa.UseRecoveryCode(ctx, ws, userID, RecoveryCodeHash(userID, recovery), now)
		return "recoveryCode", ok, err
	}
	st, err := s.mfa.GetMFA(ctx, ws, userID)
	if errors.Is(err, ErrMFANotFound) || (err == nil && st.SecretCiphertext == "") {
		return "totp", false, nil
	}
	if err != nil {
		return "totp", false, err
	}
	secret, err := s.sealer.Open(st.SecretCiphertext, MFASecretContext(userID))
	if err != nil {
		return "totp", false, err
	}
	step, ok := MatchTOTP(secret, code, now)
	if !ok || step <= st.LastStep {
		return "totp", false, nil
	}
	advanced, err := s.mfa.AdvanceStep(ctx, ws, userID, step)
	return "totp", advanced, err
}

// MFAStatus is a user's MFA state as shown to that user.
type MFAStatus struct {
	Enabled           bool
	EnabledAt         *time.Time
	RecoveryCodesLeft int
	Required          bool // the installation policy requires MFA for this user
	EnrolmentRequired bool // required but not enabled: other API use is blocked
	PendingEnrolment  bool
	PendingExpiresAt  *time.Time
}

// Status returns the caller's MFA state.
func (s *Service) Status(ctx context.Context, p access.Principal) (MFAStatus, error) {
	if err := p.RequireUser(); err != nil {
		return MFAStatus{}, err
	}
	if s.mfa == nil {
		return MFAStatus{}, errMFADisabled()
	}
	u, err := s.users.Get(ctx, p.WorkspaceId, p.Id)
	if err != nil {
		return MFAStatus{}, err
	}
	policy := s.policy()
	out := MFAStatus{Enabled: u.MFAEnabled(), EnabledAt: u.MFAEnabledAt,
		Required: policy.RequireAdminMFA && u.IsAdmin(), EnrolmentRequired: mfaEnrolmentRequired(policy, u)}
	st, err := s.mfa.GetMFA(ctx, p.WorkspaceId, p.Id)
	if err != nil && !errors.Is(err, ErrMFANotFound) {
		return MFAStatus{}, err
	}
	if err == nil {
		if out.Enabled {
			out.RecoveryCodesLeft = len(st.RecoveryHashes)
		}
		if st.PendingCiphertext != "" && st.PendingExpiresAt != nil && s.clock.Now().Before(*st.PendingExpiresAt) {
			out.PendingEnrolment, out.PendingExpiresAt = true, st.PendingExpiresAt
		}
	}
	return out, nil
}

// reauthenticate verifies the caller's current password for a sensitive
// account change (throttled per user).
func (s *Service) reauthenticate(ctx context.Context, p access.Principal, password string) (users.User, error) {
	var v validation.Collector
	v.String(validation.AuthPassword, "password", password)
	if err := v.Err(); err != nil {
		return users.User{}, err
	}
	keys := []string{"reauth:" + p.Id}
	tooMany := func(wait time.Duration) error {
		return fault.RateLimited("tooManyAttempts", "Too many attempts. Try again later.", wait)
	}
	if wait, err := s.throttle.Check(ctx, keys); err != nil {
		return users.User{}, err
	} else if wait > 0 {
		return users.User{}, tooMany(wait)
	}
	u, err := s.users.Get(ctx, p.WorkspaceId, p.Id)
	if err != nil {
		return users.User{}, err
	}
	if wait, err := s.throttle.Begin(ctx, keys); err != nil {
		return users.User{}, err
	} else if wait > 0 {
		return users.User{}, tooMany(wait)
	}
	if err := s.passwords.Verify(password, u.PasswordHash); err != nil {
		s.appendAudit(ctx, p.WorkspaceId, p.AuditActor(), audit.AuthReauthenticate, "user/"+p.Id, "failure", "")
		return users.User{}, fault.Field("password", "incorrect", "The password is incorrect.")
	}
	_ = s.throttle.Reset(ctx, keys)
	return u, nil
}

// stepUp requires a current second factor (TOTP code, replay-protected, or an
// unused recovery code, which is consumed) for a sensitive MFA change, on top
// of the password. Wrong codes count against a per-user throttle.
func (s *Service) stepUp(ctx context.Context, p access.Principal, code, recovery string) error {
	var v validation.Collector
	switch {
	case code != "" && recovery != "":
		v.Add("code", validation.CodeInvalidChoice, "Send either a code or a recovery code, not both.")
	case code != "":
		v.String(validation.AuthMFACode, "code", code)
	case recovery != "":
		v.String(validation.AuthRecoveryCode, "recoveryCode", recovery)
	default:
		v.Add("code", validation.CodeRequired, "Enter the code from your authenticator app or a recovery code.")
	}
	if err := v.Err(); err != nil {
		return err
	}
	// Same key as the sign-in second step: one guessing budget per account.
	keys := []string{"mfa:user:" + p.Id}
	if wait, err := s.throttle.Check(ctx, keys); err != nil {
		return err
	} else if wait > 0 {
		return fault.RateLimited("mfaThrottled", "Too many incorrect codes. Try again later.", wait)
	}
	if wait, err := s.throttle.Begin(ctx, keys); err != nil {
		return err
	} else if wait > 0 {
		return fault.RateLimited("mfaThrottled", "Too many incorrect codes. Try again later.", wait)
	}
	method, ok, err := s.verifySecondFactor(ctx, p.WorkspaceId, p.Id, code, recovery, s.clock.Now())
	if err != nil {
		return err
	}
	if !ok {
		s.appendAudit(ctx, p.WorkspaceId, p.AuditActor(), audit.AuthMFA, "user/"+p.Id, "failure", method)
		return fault.Field("code", "incorrect", "The code is incorrect or has already been used.")
	}
	_ = s.throttle.Reset(ctx, keys)
	return nil
}

// Enrolment is a new, unconfirmed TOTP secret. Secret and URI are shown
// once; the URI is suitable for a QR code.
type Enrolment struct {
	Secret    string
	URI       string
	ExpiresAt time.Time
}

// BeginEnrolment creates a TOTP secret for the caller after password
// re-authentication. It is confirmed with ConfirmEnrolment.
func (s *Service) BeginEnrolment(ctx context.Context, p access.Principal, password string) (Enrolment, error) {
	if err := p.RequireUser(); err != nil {
		return Enrolment{}, err
	}
	if s.mfa == nil {
		return Enrolment{}, errMFADisabled()
	}
	u, err := s.reauthenticate(ctx, p, password)
	if err != nil {
		return Enrolment{}, err
	}
	if u.MFAEnabled() {
		return Enrolment{}, fault.Conflict("mfaAlreadyEnabled", "Multi-factor authentication is already enabled. Disable it first to enrol a new authenticator.")
	}
	secret := NewTOTPSecret()
	sealed, err := s.sealer.Seal(secret, MFASecretContext(u.Id))
	if err != nil {
		return Enrolment{}, err
	}
	now := s.clock.Now()
	exp := now.Add(MFAEnrolmentTTL)
	ev := audit.New(ctx, p.AuditActor(), audit.UserMFAEnrolmentStarted, "user/"+u.Id, now)
	ev.WorkspaceId = p.WorkspaceId
	if err := s.mfa.SavePendingMFA(ctx, MFAState{UserId: u.Id, WorkspaceId: p.WorkspaceId, PendingCiphertext: sealed,
		PendingExpiresAt: &exp, UpdatedAt: now}, ev); err != nil {
		return Enrolment{}, err
	}
	issuer := s.policy().Issuer
	return Enrolment{Secret: EncodeTOTPSecret(secret), URI: OTPAuthURI(issuer, u.Email, secret), ExpiresAt: exp}, nil
}

// ConfirmEnrolment verifies a code from the new authenticator, enables MFA,
// revokes the caller's other sessions and returns the recovery codes
// (shown once).
func (s *Service) ConfirmEnrolment(ctx context.Context, p access.Principal, code string) ([]string, error) {
	if err := p.RequireUser(); err != nil {
		return nil, err
	}
	if s.mfa == nil {
		return nil, errMFADisabled()
	}
	var v validation.Collector
	v.String(validation.AuthMFACode, "code", code)
	if err := v.Err(); err != nil {
		return nil, err
	}
	keys := []string{"mfa:enrol:" + p.Id}
	if wait, err := s.throttle.Check(ctx, keys); err != nil {
		return nil, err
	} else if wait > 0 {
		return nil, fault.RateLimited("mfaThrottled", "Too many incorrect codes. Try again later.", wait)
	}
	now := s.clock.Now()
	st, err := s.mfa.GetMFA(ctx, p.WorkspaceId, p.Id)
	if errors.Is(err, ErrMFANotFound) || (err == nil && (st.PendingCiphertext == "" || st.PendingExpiresAt == nil || !now.Before(*st.PendingExpiresAt))) {
		return nil, fault.Conflict("mfaEnrolmentNotStarted", "Start enrolment again; the setup key has expired or was not created.")
	}
	if err != nil {
		return nil, err
	}
	if st.EnabledAt != nil {
		return nil, fault.Conflict("mfaAlreadyEnabled", "Multi-factor authentication is already enabled.")
	}
	secret, err := s.sealer.Open(st.PendingCiphertext, MFASecretContext(p.Id))
	if err != nil {
		return nil, err
	}
	if wait, err := s.throttle.Begin(ctx, keys); err != nil {
		return nil, err
	} else if wait > 0 {
		return nil, fault.RateLimited("mfaThrottled", "Too many incorrect codes. Try again later.", wait)
	}
	step, ok := MatchTOTP(secret, code, now)
	if !ok {
		s.appendAudit(ctx, p.WorkspaceId, p.AuditActor(), audit.UserMFAEnrolmentConfirm, "user/"+p.Id, "failure", "")
		return nil, fault.Field("code", "incorrect", "The code is incorrect. Check the time on your device and try again.")
	}
	codes := NewRecoveryCodes(RecoveryCodeCount)
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, RecoveryCodeHash(p.Id, c))
	}
	ev := audit.New(ctx, p.AuditActor(), audit.UserMFAEnabled, "user/"+p.Id, now)
	ev.WorkspaceId = p.WorkspaceId
	if err := s.mfa.EnableMFA(ctx, p.WorkspaceId, p.Id, st.PendingCiphertext, step, hashes, p.SessionId, now, ev); err != nil {
		if errors.Is(err, ErrMFANotFound) {
			return nil, fault.Conflict("mfaEnrolmentNotStarted", "Start enrolment again; the setup key has changed.")
		}
		return nil, err
	}
	_ = s.throttle.Reset(ctx, keys)
	return codes, nil
}

// RegenerateRecoveryCodes replaces the caller's recovery codes after
// password re-authentication plus a current TOTP code or an unused recovery code, and returns the new codes (shown once).
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, p access.Principal, password, code, recoveryCode string) ([]string, error) {
	if err := p.RequireUser(); err != nil {
		return nil, err
	}
	if s.mfa == nil {
		return nil, errMFADisabled()
	}
	u, err := s.reauthenticate(ctx, p, password)
	if err != nil {
		return nil, err
	}
	if !u.MFAEnabled() {
		return nil, fault.Conflict("mfaNotEnabled", "Multi-factor authentication is not enabled.")
	}
	if err := s.stepUp(ctx, p, code, recoveryCode); err != nil {
		return nil, err
	}
	codes := NewRecoveryCodes(RecoveryCodeCount)
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, RecoveryCodeHash(u.Id, c))
	}
	now := s.clock.Now()
	ev := audit.New(ctx, p.AuditActor(), audit.UserMFARecoveryCodesRegenerated, "user/"+u.Id, now)
	ev.WorkspaceId = p.WorkspaceId
	if err := s.mfa.ReplaceRecoveryCodes(ctx, p.WorkspaceId, u.Id, hashes, now, ev); err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableMFA turns MFA off for the caller after password re-authentication
// plus a current TOTP code or an unused recovery code.
// Administrators cannot disable it while the installation requires it.
func (s *Service) DisableMFA(ctx context.Context, p access.Principal, password, code, recoveryCode string) error {
	if err := p.RequireUser(); err != nil {
		return err
	}
	if s.mfa == nil {
		return errMFADisabled()
	}
	u, err := s.reauthenticate(ctx, p, password)
	if err != nil {
		return err
	}
	if !u.MFAEnabled() {
		return fault.Conflict("mfaNotEnabled", "Multi-factor authentication is not enabled.")
	}
	if s.policy().RequireAdminMFA && u.IsAdmin() {
		return fault.Conflict("mfaRequiredByPolicy", "This installation requires administrators to use multi-factor authentication.")
	}
	if err := s.stepUp(ctx, p, code, recoveryCode); err != nil {
		return err
	}
	now := s.clock.Now()
	ev := audit.New(ctx, p.AuditActor(), audit.UserMFADisabled, "user/"+u.Id, now)
	ev.WorkspaceId = p.WorkspaceId
	return s.mfa.DisableMFA(ctx, p.WorkspaceId, u.Id, now, ev)
}

// ResetUserMFA lets an administrator remove another user's MFA (lost
// device and no recovery codes). The user can sign in with the password
// alone afterwards and should enrol again.
func (s *Service) ResetUserMFA(ctx context.Context, p access.Principal, userID string) error {
	if err := p.RequireAdmin(); err != nil {
		return err
	}
	if s.mfa == nil {
		return errMFADisabled()
	}
	if !validation.ValidID(userID) {
		return fault.NotFound("userNotFound")
	}
	if userID == p.Id {
		return fault.Conflict("cannotResetOwnMfa", "Use your own security settings to change your MFA.")
	}
	u, err := s.users.Get(ctx, p.WorkspaceId, userID)
	if errors.Is(err, users.ErrNotFound) {
		return fault.NotFound("userNotFound")
	}
	if err != nil {
		return err
	}
	if !u.MFAEnabled() {
		return fault.Conflict("mfaNotEnabled", "This user has not enabled multi-factor authentication.")
	}
	now := s.clock.Now()
	ev := audit.New(ctx, p.AuditActor(), audit.UserMFAReset, "user/"+u.Id, now)
	ev.WorkspaceId = p.WorkspaceId
	return s.mfa.DisableMFA(ctx, p.WorkspaceId, u.Id, now, ev)
}
