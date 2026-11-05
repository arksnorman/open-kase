package auth

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/users"
)

// Identity repository errors.
var (
	ErrMFANotFound          = errors.New("auth: mfa enrolment not found")
	ErrChallengeNotFound    = errors.New("auth: mfa challenge not found")
	ErrAccountTokenInvalid  = errors.New("auth: account token invalid, used, revoked or expired")
	ErrAccountStateMismatch = errors.New("auth: account is not in the state the token requires")
)

// MFAState is a user's TOTP enrolment. Secrets are sealed with the master
// key (context MFASecretContext(userID)); recovery codes are stored as
// digests only.
type MFAState struct {
	UserId      string
	WorkspaceId string
	// SecretCiphertext is the confirmed secret (empty until confirmed).
	SecretCiphertext string
	// PendingCiphertext is a secret awaiting confirmation.
	PendingCiphertext string
	PendingExpiresAt  *time.Time
	EnabledAt         *time.Time
	// LastStep is the last accepted TOTP time step; codes for this step or
	// earlier are rejected so an observed code cannot be replayed.
	LastStep       int64
	RecoveryHashes []string
	UpdatedAt      time.Time
}

// MFASecretContext binds a sealed TOTP secret to its user.
func MFASecretContext(userID string) string { return "auth.mfaTotpSecret:" + userID }

// MFAChallenge is the short-lived state between a correct password and the
// second factor. Only the token digest is stored.
type MFAChallenge struct {
	Id          string
	WorkspaceId string
	UserId      string
	TokenHash   string
	Attempts    int
	CreatedAt   time.Time
	ExpiresAt   time.Time
	UsedAt      *time.Time
	UserAgent   string
	ClientIP    string
}

// MFARepository persists TOTP enrolments and login challenges.
type MFARepository interface {
	GetMFA(ctx context.Context, workspaceID, userID string) (MFAState, error)
	// SavePendingMFA stores a secret awaiting confirmation (replacing an
	// earlier pending one) and appends ev.
	SavePendingMFA(ctx context.Context, st MFAState, ev audit.Event) error
	// EnableMFA promotes the pending secret, stores the recovery digests,
	// sets the user's mfaEnabledAt, revokes the user's other sessions and
	// appends ev — atomically. It fails with ErrMFANotFound when no pending
	// secret matches pendingCiphertext.
	EnableMFA(ctx context.Context, workspaceID, userID, pendingCiphertext string, step int64, recoveryHashes []string,
		keepSessionID string, now time.Time, ev audit.Event) error
	// DisableMFA removes the enrolment, clears mfaEnabledAt and appends ev.
	DisableMFA(ctx context.Context, workspaceID, userID string, now time.Time, ev audit.Event) error
	// ReplaceRecoveryCodes stores a new set of recovery digests.
	ReplaceRecoveryCodes(ctx context.Context, workspaceID, userID string, hashes []string, now time.Time, ev audit.Event) error
	// UseRecoveryCode atomically removes hash; false when it was not present.
	UseRecoveryCode(ctx context.Context, workspaceID, userID, hash string, now time.Time) (bool, error)
	// AdvanceStep records step as used; false when step ≤ the last used step.
	AdvanceStep(ctx context.Context, workspaceID, userID string, step int64) (bool, error)

	CreateChallenge(ctx context.Context, ch MFAChallenge) (MFAChallenge, error)
	ChallengeByHash(ctx context.Context, tokenHash string) (MFAChallenge, error)
	// ChallengeAttempt counts one attempt on the challenge atomically and
	// returns the new count (checked against MFAChallengeMaxAttempts before
	// the code is evaluated).
	ChallengeAttempt(ctx context.Context, id string) (int, error)
	// ConsumeChallenge marks the challenge used; false when it was already
	// used (a concurrent completion won).
	ConsumeChallenge(ctx context.Context, id string, now time.Time) (bool, error)
}

// Account token purposes.
const (
	PurposePasswordReset = "passwordReset"
	PurposeInvitation    = "invitation"
)

// AccountToken is a single-use emailed token (password reset or
// invitation). Only the SHA-256 digest of the token is stored.
type AccountToken struct {
	Id          string
	WorkspaceId string
	UserId      string
	Purpose     string
	TokenHash   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	UsedAt      *time.Time
	RevokedAt   *time.Time
	CreatedBy   audit.Actor
	// DeliveryID is the email delivery (and job) queued with the token.
	DeliveryId string
}

// AccountTokenContext binds the sealed copy of a token (held in its email
// job until delivery) to the token record.
func AccountTokenContext(tokenID string) string { return "auth.accountToken:" + tokenID }

// AccountMail asks the repository to enqueue the email job and its delivery
// record in the same transaction as the token.
type AccountMail struct {
	Template    string
	Address     string
	TokenSealed string
}

// AccountCompletion is applied to the user when a token is redeemed.
type AccountCompletion struct {
	PasswordHash string
	// Name, when set, replaces the name parts and display name
	// (invitation acceptance).
	Name *users.PersonName
	// RequireStatus is the user status the token's purpose requires
	// (active for reset, invited for invitations).
	RequireStatus string
	// Activate sets the user active and marks the email verified.
	Activate bool
}

// AccountTokenRepository persists account tokens.
type AccountTokenRepository interface {
	// IssueAccountToken revokes the user's earlier unused tokens of the same
	// purpose, stores tok (assigning its ID before mail is built through
	// build), enqueues the email job + delivery and appends ev, atomically.
	IssueAccountToken(ctx context.Context, tok AccountToken, build func(tokenID string) (AccountMail, error), ev audit.Event) (AccountToken, error)
	// RedeemAccountToken marks the token used and applies c to its user,
	// revoking every session of the user and appending ev, atomically. It
	// returns ErrAccountTokenInvalid or ErrAccountStateMismatch.
	RedeemAccountToken(ctx context.Context, tokenHash, purpose string, now time.Time, c AccountCompletion, ev audit.Event) (userID string, err error)
	// RevokeAccountTokens revokes the user's unused tokens of purpose and
	// returns how many were revoked; ev is appended when any were.
	RevokeAccountTokens(ctx context.Context, workspaceID, userID, purpose string, now time.Time, ev audit.Event) (int, error)
	// AccountTokenUsable reports whether an unused, unrevoked, unexpired
	// token with this hash and purpose exists (a cheap pre-check before
	// hashing the new password; RedeemAccountToken re-checks atomically).
	AccountTokenUsable(ctx context.Context, tokenHash, purpose string, now time.Time) (bool, error)
	// PendingAccountToken returns the newest unused, unexpired token.
	PendingAccountToken(ctx context.Context, workspaceID, userID, purpose string, now time.Time) (AccountToken, error)
}
