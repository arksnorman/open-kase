package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Session is one sign-in: a refresh-token family plus its CSRF binding.
type Session struct {
	Id            string
	WorkspaceId   string
	UserId        string
	CSRFHash      string
	CreatedAt     time.Time
	LastUsedAt    time.Time
	ExpiresAt     time.Time // absolute lifetime of the family
	RevokedAt     *time.Time
	RevokedReason string
	UserAgent     string
	ClientIP      string
}

// Valid reports whether the session can authenticate requests at now.
func (s Session) Valid(now time.Time) bool { return s.RevokedAt == nil && now.Before(s.ExpiresAt) }

// RefreshGrace is how long the immediately previous refresh token is still
// honoured after its rotation, provided nobody has used its successor yet.
// Browser tabs share one refresh cookie, so two tabs whose access tokens
// expire together (or that are opened at once) present the same token at
// the same moment; without the grace window the slower one would be taken
// for token theft and sign the user out everywhere. Inside the window such a
// presentation yields an access token for the same session but no refresh
// token: the browser keeps the successor the first response set. A token
// presented after the window, or after its successor was used, is a replay
// and revokes the session family.
const RefreshGrace = 10 * time.Second

// RotateOutcome is the result of presenting a refresh token.
type RotateOutcome int

const (
	// Rotated: the token was current; it is now used and NewHash is current.
	Rotated RotateOutcome = iota + 1
	// Invalid: unknown token, or the session is revoked or expired.
	Invalid
	// Reused: an already-rotated token was presented again; the whole
	// family (session) has been revoked and the event audited.
	Reused
	// Replayed: the immediately previous token was presented again inside
	// the grace window while its successor is still unused (two browser
	// tabs sharing one cookie). Nothing was rotated or revoked; the caller
	// issues an access token only.
	Replayed
)

// Session repository errors.
var ErrSessionNotFound = errors.New("auth: session not found")

// SessionRepository persists sessions and refresh tokens. Only SHA-256
// hashes of refresh tokens are stored.
type SessionRepository interface {
	CreateSession(ctx context.Context, s Session, refreshHash string, ev audit.Event) (Session, error)
	// SessionByRefresh finds the session a refresh token belongs to.
	SessionByRefresh(ctx context.Context, refreshHash string) (Session, error)
	// RotateRefresh atomically marks presentedHash used and stores newHash.
	// When presentedHash was already used it revokes the session and
	// appends reuseEvent (target and actor are filled in) instead — unless
	// it was used at most grace ago and its successor is still unused, in
	// which case nothing changes and the outcome is Replayed.
	RotateRefresh(ctx context.Context, presentedHash, newHash string, now time.Time, grace time.Duration, reuseEvent audit.Event) (Session, RotateOutcome, error)
	GetSession(ctx context.Context, id string) (Session, error)
	// RevokeSession marks the session revoked (idempotent) with an audit event.
	RevokeSession(ctx context.Context, id, reason string, now time.Time, ev audit.Event) error
}

// UserStore is the subset of the user repository that authentication needs.
type UserStore interface {
	Get(ctx context.Context, workspaceID, id string) (users.User, error)
	GetByEmail(ctx context.Context, workspaceID, emailNormalized string) (users.User, error)
	GetByUsername(ctx context.Context, workspaceID, username string) (users.User, error)
	Apply(ctx context.Context, c users.Change) (users.User, error)
}

// Auditor appends standalone audit events (those without a business
// mutation, such as failed sign-ins).
type Auditor interface {
	Append(ctx context.Context, ev audit.Event) error
}

// Throttle limits failed sign-in attempts. It is backed by MongoDB so that
// it holds across restarts and replicas without Redis.
//
// Callers Check first (a read: a blocked request never counts), then Begin
// to count the attempt atomically before evaluating it, so concurrent
// attempts cannot all slip past the check; a successful attempt Resets the
// keys that limit failures of an account (from one client) and Forgives the
// ones that only count failures (an account across all clients, the client
// IP).
type Throttle interface {
	Check(ctx context.Context, keys []string) (retryAfter time.Duration, err error)
	Begin(ctx context.Context, keys []string) (retryAfter time.Duration, err error)
	Forgive(ctx context.Context, keys []string) error
	Reset(ctx context.Context, keys []string) error
}

// Policy is the narrow settings view authentication needs.
type Policy struct {
	AccessTTL  time.Duration
	RefreshTTL time.Duration // absolute session lifetime
	// RequireAdminMFA forces administrators to enrol TOTP before other use.
	RequireAdminMFA bool
	// Issuer names the installation in authenticator apps.
	Issuer string
}

// Service implements login, refresh rotation, logout and access-token
// authentication.
type Service struct {
	log       *slog.Logger
	users     UserStore
	audit     Auditor
	sessions  SessionRepository
	tokens    *Tokens
	passwords *Passwords
	throttle  Throttle
	clock     clock.Clock
	workspace func(ctx context.Context) (string, error)
	policy    func() Policy
	mfa       MFARepository
	accounts  AccountTokenRepository
	sealer    Sealer
	waker     Waker
}

// Waker hints the job workers that new work is queued.
type Waker interface{ Notify(ctx context.Context) }

// Deps are the service collaborators.
type Deps struct {
	Log       *slog.Logger
	Users     UserStore
	Audit     Auditor
	Sessions  SessionRepository
	Tokens    *Tokens
	Passwords *Passwords
	Throttle  Throttle
	Clock     clock.Clock
	Workspace func(ctx context.Context) (string, error)
	Policy    func() Policy
	// MFA, Accounts and Sealer enable TOTP MFA, password reset and
	// invitations. Waker (optional) wakes the email workers.
	MFA      MFARepository
	Accounts AccountTokenRepository
	Sealer   Sealer
	Waker    Waker
}

// NewService constructs the authentication service.
func NewService(d Deps) *Service {
	return &Service{log: d.Log, users: d.Users, audit: d.Audit, sessions: d.Sessions, tokens: d.Tokens, passwords: d.Passwords,
		throttle: d.Throttle, clock: d.Clock, workspace: d.Workspace, policy: d.Policy,
		mfa: d.MFA, accounts: d.Accounts, sealer: d.Sealer, waker: d.Waker}
}

// Issued is the result of a login or refresh. RefreshToken and CSRFToken
// are for cookies only; CSRFToken is empty on refresh (it is unchanged).
// RefreshToken is empty when a refresh fell into the grace window
// (RefreshGrace): the cookie the client already has, or is about to receive
// from the concurrent request that rotated the token, stays current.
type Issued struct {
	AccessToken      string
	ExpiresIn        int
	User             users.User
	RefreshToken     string
	CSRFToken        string
	SessionExpiresAt time.Time
	// MFARequired means the password was correct but no session was
	// opened: complete sign-in with MFAToken at POST /auth/mfa.
	MFARequired  bool
	MFAToken     string
	MFAExpiresIn int
	// MFAEnrolmentRequired means the session is limited to TOTP enrolment
	// until the administrator enrols (auth.requireAdminMfa).
	MFAEnrolmentRequired bool
}

// LoginInput is the login command.
type LoginInput struct {
	// Login is an email address (contains "@") or a username.
	Login     string
	Password  string
	UserAgent string
	ClientIP  string
}

func errInvalidCredentials() error {
	return fault.Unauthenticated("invalidCredentials", "The email, username or password is incorrect.")
}

func errRefresh(code string) error {
	return fault.Unauthenticated(code, "Your session has ended. Sign in again.")
}

// loginKeys are the throttle keys of one sign-in attempt (see Login).
type loginKeys struct {
	// perClient count failures of the identifier and of the resolved
	// account from this client (throttleClient); a successful sign-in
	// clears them.
	perClient []string
	// failuresOnly count failures of the identifier and of the account from
	// every client (the backstop against guessing spread over many
	// addresses) and of the client IP. A successful sign-in takes back only
	// its own attempt and never clears them, so interleaving correct
	// sign-ins cannot reopen a distributed brute force.
	failuresOnly []string
}

// newLoginKeys returns the keys for the normalized identifier ident, the
// account it resolves to (userID, "" when unknown) and the client IP (""
// when unknown: such requests share one client per identifier and account).
func newLoginKeys(ident, userID, clientIP string) loginKeys {
	id, client := digest(ident), throttleClient(clientIP)
	k := loginKeys{perClient: []string{"login:id-ip:" + id + ":" + client}, failuresOnly: []string{"login:id:" + id}}
	if userID != "" {
		k.perClient = append(k.perClient, "login:user-ip:"+userID+":"+client)
		k.failuresOnly = append(k.failuresOnly, "login:user:"+userID)
	}
	if clientIP != "" {
		k.failuresOnly = append(k.failuresOnly, "login:ip:"+clientIP)
	}
	return k
}

func (k loginKeys) all() []string {
	return append(append(make([]string, 0, len(k.perClient)+len(k.failuresOnly)), k.perClient...), k.failuresOnly...)
}

// throttleClient names the client of a per-client sign-in key: the IPv4
// address, or the /64 network of an IPv6 address. A host or site is given
// a whole /64, so its addresses are one client; otherwise a single IPv6 host
// could rotate addresses past the per-client limit and reach the
// account-wide one alone.
func throttleClient(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr = addr.Unmap(); addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// Login verifies credentials and opens a session. The identifier is an
// email address when it contains "@" and a username otherwise. Unknown
// accounts, wrong passwords, disabled and invited accounts all return the
// same error.
//
// Failures count toward the normalized identifier and the resolved account
// (so alternating between a user's email and username shares one limit),
// each per client and across all clients, and toward the client IP. The
// per-client limit is low and the all-clients limit high: a guesser locks
// only their own client out of an account, while guessing spread over many
// clients still meets a ceiling (mongo.DefaultThrottleRule).
func (s *Service) Login(ctx context.Context, in LoginInput) (Issued, error) {
	var v validation.Collector
	v.String(validation.AuthLogin, "login", in.Login)
	v.String(validation.AuthPassword, "password", in.Password)
	if err := v.Err(); err != nil {
		return Issued{}, err
	}
	ws, err := s.workspace(ctx)
	if err != nil {
		return Issued{}, err
	}
	var u users.User
	ident := users.NormalizeEmail(in.Login) // trimmed, lower case; also the username normal form
	if users.IsEmailIdentifier(ident) {
		u, err = s.users.GetByEmail(ctx, ws, ident)
	} else {
		u, err = s.users.GetByUsername(ctx, ws, users.NormalizeUsername(ident))
	}
	if err != nil && !errors.Is(err, users.ErrNotFound) {
		return Issued{}, err
	}
	lk := newLoginKeys(ident, u.Id, in.ClientIP)
	keys := lk.all()
	throttled := func(wait time.Duration) error {
		return fault.RateLimited("loginThrottled", "Too many failed sign-in attempts. Try again later.", wait)
	}
	wait, err := s.throttle.Check(ctx, keys)
	if err != nil {
		return Issued{}, err
	}
	if wait > 0 {
		return Issued{}, throttled(wait)
	}
	// Count the attempt before checking the password, so that a burst of
	// concurrent guesses cannot all pass the check above.
	if wait, err = s.throttle.Begin(ctx, keys); err != nil {
		return Issued{}, err
	} else if wait > 0 {
		return Issued{}, throttled(wait)
	}

	// An unknown account has an empty hash, which still costs one hash
	// computation, and is never Active.
	needsRehash, verr := s.passwords.VerifyAndCheck(in.Password, u.PasswordHash)
	if u.Id == "" || verr != nil || !u.Active() {
		if u.Id != "" {
			ev := audit.New(ctx, audit.Actor{Type: audit.ActorUser, Id: u.Id}, audit.AuthLogin, "user/"+u.Id, s.clock.Now())
			ev.WorkspaceId, ev.Outcome = ws, "failure"
			if aerr := s.audit.Append(ctx, ev); aerr != nil {
				s.log.Warn("could not audit failed login", "error", aerr.Error())
			}
		}
		return Issued{}, errInvalidCredentials()
	}
	// Successful sign-in clears this client's identifier and account
	// counters; the all-clients and per-IP ones only count failures, so
	// this attempt is taken back from them.
	if err := s.throttle.Reset(ctx, lk.perClient); err != nil {
		s.log.Warn("could not reset login throttle", "error", err.Error())
	}
	if err := s.throttle.Forgive(ctx, lk.failuresOnly); err != nil {
		s.log.Warn("could not adjust login throttle", "error", err.Error())
	}
	if needsRehash {
		if hash, herr := s.passwords.Hash(in.Password); herr == nil {
			u.PasswordHash = hash
			if updated, aerr := s.users.Apply(ctx, users.Change{User: u, Only: users.OnlyPassword}); aerr == nil {
				u = updated
			}
		}
	}

	if u.MFAEnabled() && s.mfa != nil {
		return s.challenge(ctx, ws, u, in)
	}
	return s.openSession(ctx, ws, u, in.UserAgent, in.ClientIP, "")
}

// openSession creates a session for an authenticated user. method is
// recorded in the audit reason ("" for password only).
func (s *Service) openSession(ctx context.Context, ws string, u users.User, userAgent, clientIP, method string) (Issued, error) {
	policy := s.policy()
	now := s.clock.Now()
	refresh, refreshHash := newOpaqueToken()
	csrf, csrfHash := newOpaqueToken()
	sess := Session{WorkspaceId: ws, UserId: u.Id, CSRFHash: csrfHash, CreatedAt: now, LastUsedAt: now,
		ExpiresAt: now.Add(policy.RefreshTTL), UserAgent: truncate(userAgent, 256), ClientIP: clientIP}
	ev := audit.New(ctx, audit.Actor{Type: audit.ActorUser, Id: u.Id}, audit.AuthLogin, "user/"+u.Id, now)
	ev.WorkspaceId = ws
	if method != "" {
		ev.Reason = "second factor: " + method
	}
	sess, err := s.sessions.CreateSession(ctx, sess, refreshHash, ev)
	if err != nil {
		return Issued{}, err
	}
	access, err := s.tokens.Issue(ctx, ws, u.Id, sess.Id, policy.AccessTTL)
	if err != nil {
		return Issued{}, err
	}
	return Issued{AccessToken: access, ExpiresIn: int(policy.AccessTTL / time.Second), User: u,
		RefreshToken: refresh, CSRFToken: csrf, SessionExpiresAt: sess.ExpiresAt,
		MFAEnrolmentRequired: mfaEnrolmentRequired(policy, u)}, nil
}

// mfaEnrolmentRequired reports whether u must enrol TOTP before other use.
func mfaEnrolmentRequired(p Policy, u users.User) bool {
	return p.RequireAdminMFA && u.IsAdmin() && !u.MFAEnabled()
}

// CheckCSRF verifies that csrfToken belongs to the refresh token's session.
// The transport has already checked the double-submit header against the
// cookie and the request Origin.
func (s *Service) checkCSRF(sess Session, csrfToken string) error {
	if csrfToken == "" || subtle.ConstantTimeCompare([]byte(digest(csrfToken)), []byte(sess.CSRFHash)) != 1 {
		return fault.Forbidden("csrfFailed", "The request could not be verified. Reload the page and try again.")
	}
	return nil
}

// Refresh rotates the refresh token and issues a new access token. Reusing
// an already-rotated refresh token revokes the whole session family, except
// inside the grace window (RefreshGrace), where the immediately previous
// token yields an access token only.
func (s *Service) Refresh(ctx context.Context, refreshToken, csrfToken string) (Issued, error) {
	if refreshToken == "" {
		return Issued{}, errRefresh("refreshTokenMissing")
	}
	presented := digest(refreshToken)
	sess, err := s.sessions.SessionByRefresh(ctx, presented)
	if errors.Is(err, ErrSessionNotFound) {
		return Issued{}, errRefresh("invalidRefreshToken")
	}
	if err != nil {
		return Issued{}, err
	}
	if err := s.checkCSRF(sess, csrfToken); err != nil {
		return Issued{}, err
	}
	now := s.clock.Now()
	next, nextHash := newOpaqueToken()
	reuse := audit.New(ctx, audit.Actor{}, audit.AuthRefreshReuseDetected, "", now)
	reuse.Reason = "refresh token reused; session family revoked"
	sess, outcome, err := s.sessions.RotateRefresh(ctx, presented, nextHash, now, RefreshGrace, reuse)
	if err != nil {
		return Issued{}, err
	}
	switch outcome {
	case Reused:
		s.log.Warn("refresh token reuse detected; session revoked", "sessionId", sess.Id, "userId", sess.UserId)
		return Issued{}, errRefresh("refreshTokenReused")
	case Invalid:
		return Issued{}, errRefresh("invalidRefreshToken")
	case Replayed:
		// No second refresh token: only the response of the request that
		// rotated the token carries the successor.
		next = ""
		s.log.Debug("refresh token presented again inside the grace window; no rotation", "sessionId", sess.Id)
	}
	u, err := s.users.Get(ctx, sess.WorkspaceId, sess.UserId)
	if err != nil || !u.Active() {
		if err != nil && !errors.Is(err, users.ErrNotFound) {
			return Issued{}, err
		}
		return Issued{}, errRefresh("invalidRefreshToken")
	}
	policy := s.policy()
	access, err := s.tokens.Issue(ctx, sess.WorkspaceId, u.Id, sess.Id, policy.AccessTTL)
	if err != nil {
		return Issued{}, err
	}
	return Issued{AccessToken: access, ExpiresIn: int(policy.AccessTTL / time.Second), User: u,
		RefreshToken: next, SessionExpiresAt: sess.ExpiresAt, MFAEnrolmentRequired: mfaEnrolmentRequired(policy, u)}, nil
}

// Logout revokes the refresh token's session. It succeeds when the token is
// unknown so that clients can always clear their cookies.
func (s *Service) Logout(ctx context.Context, refreshToken, csrfToken string) error {
	if refreshToken == "" {
		return nil
	}
	sess, err := s.sessions.SessionByRefresh(ctx, digest(refreshToken))
	if errors.Is(err, ErrSessionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.checkCSRF(sess, csrfToken); err != nil {
		return err
	}
	if sess.RevokedAt != nil {
		return nil
	}
	now := s.clock.Now()
	ev := audit.New(ctx, audit.Actor{Type: audit.ActorUser, Id: sess.UserId}, audit.AuthLogout, "session/"+sess.Id, now)
	ev.WorkspaceId = sess.WorkspaceId
	return s.sessions.RevokeSession(ctx, sess.Id, "logout", now, ev)
}

// Authenticate resolves a bearer access token into a principal. Besides the
// token checks it requires the session to be current and the user active,
// so revocation and deactivation take effect on the next request.
func (s *Service) Authenticate(ctx context.Context, raw string) (access.Principal, error) {
	ws, err := s.workspace(ctx)
	if err != nil {
		return access.Principal{}, err
	}
	claims, err := s.tokens.Verify(ctx, ws, raw)
	if errors.Is(err, ErrTokenExpired) {
		return access.Principal{}, fault.Unauthenticated("tokenExpired", "The access token has expired.")
	}
	if err != nil {
		return access.Principal{}, fault.Unauthenticated("invalidToken", "The access token is not valid.")
	}
	sess, err := s.sessions.GetSession(ctx, claims.SessionId)
	if errors.Is(err, ErrSessionNotFound) {
		return access.Principal{}, fault.Unauthenticated("sessionRevoked", "The session has ended.")
	}
	if err != nil {
		return access.Principal{}, err
	}
	if !sess.Valid(s.clock.Now()) || sess.UserId != claims.Subject || sess.WorkspaceId != ws {
		return access.Principal{}, fault.Unauthenticated("sessionRevoked", "The session has ended.")
	}
	u, err := s.users.Get(ctx, ws, claims.Subject)
	if errors.Is(err, users.ErrNotFound) || (err == nil && !u.Active()) {
		return access.Principal{}, fault.Unauthenticated("accountInactive", "This account is not active.")
	}
	if err != nil {
		return access.Principal{}, err
	}
	return access.Principal{Kind: access.KindUser, Id: u.Id, WorkspaceId: ws, Name: u.Name, Email: u.Email,
		SessionId: sess.Id, Admin: u.IsAdmin(), MFAEnrolmentRequired: mfaEnrolmentRequired(s.policy(), u)}, nil
}

// newOpaqueToken returns a random URL-safe token and its SHA-256 digest.
func newOpaqueToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, digest(token)
}

// digest returns the hex SHA-256 of s. Refresh, CSRF and API tokens are
// high-entropy random values, so a fast hash is appropriate.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Digest exposes the token digest for other credential types.
func Digest(s string) string { return digest(s) }

func truncate(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}
