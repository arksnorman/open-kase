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

// Account token lifetimes.
const (
	PasswordResetTTL = 30 * time.Minute
	InvitationTTL    = 72 * time.Hour
)

// Email templates used for account tokens (templates.Kind values).
const (
	TemplatePasswordReset = "passwordReset"
	TemplateInvitation    = "invitation"
)

func errTokenInvalid() error {
	return fault.Field("token", "invalidToken", "This link is invalid, has already been used or has expired. Request a new one.")
}

// issueToken creates a token for u and queues its email.
func (s *Service) issueToken(ctx context.Context, ws string, u users.User, purpose, template string, ttl time.Duration,
	actor audit.Actor, action audit.Action) (AccountToken, error) {
	now := s.clock.Now()
	token, hash := newOpaqueToken()
	ev := audit.New(ctx, actor, action, "user/"+u.Id, now)
	ev.WorkspaceId = ws
	tok, err := s.accounts.IssueAccountToken(ctx, AccountToken{WorkspaceId: ws, UserId: u.Id, Purpose: purpose, TokenHash: hash,
		CreatedAt: now, ExpiresAt: now.Add(ttl), CreatedBy: actor}, func(tokenID string) (AccountMail, error) {
		sealed, err := s.sealer.Seal([]byte(token), AccountTokenContext(tokenID))
		if err != nil {
			return AccountMail{}, err
		}
		return AccountMail{Template: template, Address: u.Email, TokenSealed: sealed}, nil
	}, ev)
	if err != nil {
		return AccountToken{}, err
	}
	if s.waker != nil {
		s.waker.Notify(ctx)
	}
	return tok, nil
}

// RequestPasswordReset emails a single-use reset link when email belongs
// to an active user. The outcome is never revealed: unknown, disabled and
// invited accounts get the same response. Requests are throttled per
// address and per client IP whether or not the account exists.
func (s *Service) RequestPasswordReset(ctx context.Context, email, clientIP string) error {
	if s.accounts == nil {
		return errMFADisabled()
	}
	var v validation.Collector
	v.String(validation.UserEmail, "email", email)
	if err := v.Err(); err != nil {
		return err
	}
	ws, err := s.workspace(ctx)
	if err != nil {
		return err
	}
	norm := users.NormalizeEmail(email)
	keys := []string{"reset:email:" + digest(norm)}
	if clientIP != "" {
		keys = append(keys, "reset:ip:"+clientIP)
	}
	if wait, err := s.throttle.Check(ctx, keys); err != nil {
		return err
	} else if wait > 0 {
		return fault.RateLimited("resetThrottled", "Too many password reset requests. Try again later.", wait)
	}
	// Every request counts toward the limit, so the throttle behaves the
	// same for existing and unknown addresses; counting before acting keeps
	// a concurrent burst within the limit.
	if wait, err := s.throttle.Begin(ctx, keys); err != nil {
		return err
	} else if wait > 0 {
		return fault.RateLimited("resetThrottled", "Too many password reset requests. Try again later.", wait)
	}
	u, err := s.users.GetByEmail(ctx, ws, norm)
	if errors.Is(err, users.ErrNotFound) || (err == nil && !u.Active()) {
		return nil
	}
	if err != nil {
		return err
	}
	actor := audit.Actor{Type: audit.ActorUser, Id: u.Id}
	_, err = s.issueToken(ctx, ws, u, PurposePasswordReset, TemplatePasswordReset, PasswordResetTTL, actor, audit.AuthPasswordResetRequested)
	return err
}

// CompletePasswordReset sets a new password with a reset token. The token
// is single use; every session of the user is revoked.
func (s *Service) CompletePasswordReset(ctx context.Context, token, password string) error {
	if s.accounts == nil {
		return errMFADisabled()
	}
	var v validation.Collector
	v.String(validation.AuthOpaqueToken, "token", token)
	v.String(validation.UserPassword, "password", password)
	if err := v.Err(); err != nil {
		if fe := fault.As(err); len(fe.Fields) > 0 && fe.Fields[0].Field == "token" {
			return errTokenInvalid()
		}
		return err
	}
	// Check the token before the (deliberately expensive) password hash,
	// so random tokens cannot make the server hash.
	now := s.clock.Now()
	if ok, err := s.accounts.AccountTokenUsable(ctx, digest(token), PurposePasswordReset, now); err != nil {
		return err
	} else if !ok {
		return errTokenInvalid()
	}
	hash, err := s.passwords.Hash(password)
	if err != nil {
		return err
	}
	ev := audit.New(ctx, audit.Actor{Type: audit.ActorUser}, audit.AuthPasswordResetCompleted, "", now)
	ev.Changes = []string{"password"}
	_, err = s.accounts.RedeemAccountToken(ctx, digest(token), PurposePasswordReset, now,
		AccountCompletion{PasswordHash: hash, RequireStatus: users.StatusActive}, ev)
	if errors.Is(err, ErrAccountTokenInvalid) || errors.Is(err, ErrAccountStateMismatch) {
		return errTokenInvalid()
	}
	return err
}

// Invitation is a queued invitation.
type Invitation struct {
	UserId    string
	ExpiresAt time.Time
	// DeliveryID is the email delivery record (also the job ID).
	DeliveryId string
}

// SendInvitation emails an invited user a single-use link (valid 72
// hours) to choose a password. Earlier unused invitations are revoked.
func (s *Service) SendInvitation(ctx context.Context, p access.Principal, userID string) (Invitation, error) {
	if err := p.RequireAdmin(); err != nil {
		return Invitation{}, err
	}
	if s.accounts == nil {
		return Invitation{}, errMFADisabled()
	}
	if !validation.ValidID(userID) {
		return Invitation{}, fault.NotFound("userNotFound")
	}
	u, err := s.users.Get(ctx, p.WorkspaceId, userID)
	if errors.Is(err, users.ErrNotFound) {
		return Invitation{}, fault.NotFound("userNotFound")
	}
	if err != nil {
		return Invitation{}, err
	}
	if u.Status != users.StatusInvited {
		return Invitation{}, fault.Conflict("userNotInvited", "Only users who have not accepted an invitation yet can be invited.")
	}
	tok, err := s.issueToken(ctx, p.WorkspaceId, u, PurposeInvitation, TemplateInvitation, InvitationTTL, p.AuditActor(), audit.UserInvitationSent)
	if err != nil {
		return Invitation{}, err
	}
	return Invitation{UserId: u.Id, ExpiresAt: tok.ExpiresAt, DeliveryId: tok.DeliveryId}, nil
}

// RevokeInvitation invalidates a user's unused invitation links.
func (s *Service) RevokeInvitation(ctx context.Context, p access.Principal, userID string) error {
	if err := p.RequireAdmin(); err != nil {
		return err
	}
	if s.accounts == nil {
		return errMFADisabled()
	}
	if !validation.ValidID(userID) {
		return fault.NotFound("userNotFound")
	}
	if _, err := s.users.Get(ctx, p.WorkspaceId, userID); errors.Is(err, users.ErrNotFound) {
		return fault.NotFound("userNotFound")
	} else if err != nil {
		return err
	}
	now := s.clock.Now()
	ev := audit.New(ctx, p.AuditActor(), audit.UserInvitationRevoked, "user/"+userID, now)
	ev.WorkspaceId = p.WorkspaceId
	n, err := s.accounts.RevokeAccountTokens(ctx, p.WorkspaceId, userID, PurposeInvitation, now, ev)
	if err != nil {
		return err
	}
	if n == 0 {
		return fault.NotFound("invitationNotFound")
	}
	return nil
}

// PendingInvitation returns the user's current invitation, if any.
func (s *Service) PendingInvitation(ctx context.Context, p access.Principal, userID string) (Invitation, error) {
	if err := p.RequireAdmin(); err != nil {
		return Invitation{}, err
	}
	if s.accounts == nil {
		return Invitation{}, errMFADisabled()
	}
	if !validation.ValidID(userID) {
		return Invitation{}, fault.NotFound("invitationNotFound")
	}
	tok, err := s.accounts.PendingAccountToken(ctx, p.WorkspaceId, userID, PurposeInvitation, s.clock.Now())
	if errors.Is(err, ErrAccountTokenInvalid) {
		return Invitation{}, fault.NotFound("invitationNotFound")
	}
	if err != nil {
		return Invitation{}, err
	}
	return Invitation{UserId: userID, ExpiresAt: tok.ExpiresAt, DeliveryId: tok.DeliveryId}, nil
}

// AcceptInvitation activates an invited user with the chosen password (and
// optionally a corrected display name). The token is single use.
func (s *Service) AcceptInvitation(ctx context.Context, token string, name users.NamePatch, password string) error {
	if s.accounts == nil {
		return errMFADisabled()
	}
	var v validation.Collector
	v.String(validation.AuthOpaqueToken, "token", token)
	// A corrected name replaces the whole name: given and family names
	// are then required, the middle name optional.
	var corrected *users.PersonName
	if !name.Empty() {
		n := name.Apply(users.PersonName{})
		n.Validate(&v, "")
		corrected = &n
	}
	v.String(validation.UserPassword, "password", password)
	if err := v.Err(); err != nil {
		if fe := fault.As(err); len(fe.Fields) > 0 && fe.Fields[0].Field == "token" {
			return errTokenInvalid()
		}
		return err
	}
	now := s.clock.Now()
	if ok, err := s.accounts.AccountTokenUsable(ctx, digest(token), PurposeInvitation, now); err != nil {
		return err
	} else if !ok {
		return errTokenInvalid()
	}
	hash, err := s.passwords.Hash(password)
	if err != nil {
		return err
	}
	ev := audit.New(ctx, audit.Actor{Type: audit.ActorUser}, audit.UserInvitationAccepted, "", now)
	ev.Changes = []string{"password", "status"}
	if corrected != nil {
		ev.Changes = append(ev.Changes, "name")
	}
	_, err = s.accounts.RedeemAccountToken(ctx, digest(token), PurposeInvitation, now,
		AccountCompletion{PasswordHash: hash, Name: corrected, RequireStatus: users.StatusInvited, Activate: true}, ev)
	if errors.Is(err, ErrAccountTokenInvalid) || errors.Is(err, ErrAccountStateMismatch) {
		return errTokenInvalid()
	}
	return err
}
