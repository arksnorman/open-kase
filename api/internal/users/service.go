package users

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Repository errors.
var (
	ErrNotFound        = errors.New("users: not found")
	ErrEmailTaken      = errors.New("users: email already in use")
	ErrUsernameTaken   = errors.New("users: username already in use")
	ErrVersionConflict = errors.New("users: version conflict")
	ErrLastAdmin       = errors.New("users: last active administrator")
)

// Change is an atomic user update. The repository applies it only when the
// stored version equals ExpectedVersion (0 skips the check), and in the same
// transaction revokes sessions and appends the audit event.
type Change struct {
	User User // the complete new state
	// Only limits the write to one field set so a self-service change can
	// never overwrite a concurrent administrative edit (status, roles).
	// Empty writes the whole administrative record.
	Only            ChangeScope
	ExpectedVersion int64
	// BumpVersion increments the version (administrative edits do;
	// personal preferences do not).
	BumpVersion bool
	// RevokeSessions revokes every session of the user except KeepSessionID.
	RevokeSessions bool
	KeepSessionId  string
	// GuardLastAdmin rejects the change with ErrLastAdmin when it would
	// leave the workspace without an active administrator.
	GuardLastAdmin bool
	// Disabled records a durable audit.UserDisabled event in the same
	// transaction so a job releases the user's ticket assignments.
	Disabled bool
	Event    *audit.Event
}

// ChangeScope selects the fields a Change writes.
type ChangeScope string

const (
	// OnlyTheme writes the theme preference only.
	OnlyTheme ChangeScope = "theme"
	// OnlyPassword writes the password hash and change time only.
	OnlyPassword ChangeScope = "password"
	// OnlyPreferences writes the theme and email preferences only.
	OnlyPreferences ChangeScope = "preferences"
	// OnlyProfile writes the display name only (self-service).
	OnlyProfile ChangeScope = "profile"
)

// Repository persists users.
type Repository interface {
	// Insert stores a new user, assigning its ID (and the event target).
	Insert(ctx context.Context, u User, ev audit.Event) (User, error)
	Get(ctx context.Context, workspaceID, id string) (User, error)
	GetByEmail(ctx context.Context, workspaceID, emailNormalized string) (User, error)
	GetByUsername(ctx context.Context, workspaceID, username string) (User, error)
	List(ctx context.Context, workspaceID string, after *pagination.Cursor, limit int) ([]User, error)
	Apply(ctx context.Context, c Change) (User, error)
}

// Passwords hashes and verifies passwords.
type Passwords interface {
	Hash(password string) (string, error)
	Verify(password, hash string) error
}

// Throttle limits password guessing on sensitive account actions: Check
// (read-only), Begin (counts the attempt atomically before it is evaluated)
// and Reset after success.
type Throttle interface {
	Check(ctx context.Context, keys []string) (retryAfter time.Duration, err error)
	Begin(ctx context.Context, keys []string) (retryAfter time.Duration, err error)
	Reset(ctx context.Context, keys []string) error
}

// Service implements user administration and self-service.
type Service struct {
	repo      Repository
	passwords Passwords
	throttle  Throttle
	clock     clock.Clock
}

// NewService constructs the users service.
func NewService(repo Repository, passwords Passwords, throttle Throttle, clk clock.Clock) *Service {
	return &Service{repo: repo, passwords: passwords, throttle: throttle, clock: clk}
}

func notFound() error { return fault.NotFound("userNotFound") }

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return notFound()
	case errors.Is(err, ErrEmailTaken):
		return fault.Conflict("emailTaken", "A user with this email address already exists.")
	case errors.Is(err, ErrUsernameTaken):
		return fault.Conflict("usernameTaken", "A user with this username already exists.")
	case errors.Is(err, ErrVersionConflict):
		return fault.Conflict("versionConflict", "This user was changed by someone else. Reload and try again.")
	case errors.Is(err, ErrLastAdmin):
		return fault.Conflict("lastAdmin", "At least one active administrator must remain.")
	}
	return err
}

// Get returns a user for an administrator.
func (s *Service) Get(ctx context.Context, p access.Principal, id string) (User, error) {
	if err := p.RequireAdmin(); err != nil {
		return User{}, err
	}
	if !validation.ValidID(id) {
		return User{}, notFound()
	}
	u, err := s.repo.Get(ctx, p.WorkspaceId, id)
	return u, mapErr(err)
}

// Me returns the calling user.
func (s *Service) Me(ctx context.Context, p access.Principal) (User, error) {
	if err := p.RequireUser(); err != nil {
		return User{}, err
	}
	u, err := s.repo.Get(ctx, p.WorkspaceId, p.Id)
	return u, mapErr(err)
}

// List pages through users for an administrator, ordered by email.
func (s *Service) List(ctx context.Context, p access.Principal, after *pagination.Cursor, limit int) (pagination.Page[User], error) {
	if err := p.RequireAdmin(); err != nil {
		return pagination.Page[User]{}, err
	}
	rows, err := s.repo.List(ctx, p.WorkspaceId, after, limit+1)
	if err != nil {
		return pagination.Page[User]{}, err
	}
	return pagination.NewPage(rows, limit, func(u User) pagination.Cursor {
		return pagination.Cursor{Sort: u.EmailNormalized, Id: u.Id}
	}), nil
}

// CreateInput is the create-user command.
type CreateInput struct {
	Email string
	// Username is optional; when nil it is derived from the email address
	// (local part, normalized; a numeric suffix is added when taken).
	Username *string
	Name     PersonName
	Password *string
	IsAdmin  bool
}

// Validate checks field rules.
func (in CreateInput) Validate() error {
	var v validation.Collector
	v.String(validation.UserEmail, "email", in.Email)
	if in.Username != nil {
		v.String(validation.UserUsername, "username", NormalizeUsername(*in.Username))
	}
	in.Name.Trimmed().Validate(&v, "")
	if in.Password != nil {
		v.String(validation.UserPassword, "password", *in.Password)
	}
	return v.Err()
}

// Create adds a staff user. Without a password the account is "invited"
// and cannot sign in until an administrator sets a password.
func (s *Service) Create(ctx context.Context, p access.Principal, in CreateInput) (User, error) {
	if err := p.RequireAdmin(); err != nil {
		return User{}, err
	}
	if err := in.Validate(); err != nil {
		return User{}, err
	}
	now := s.clock.Now()
	u := User{
		WorkspaceId: p.WorkspaceId, Email: in.Email, EmailNormalized: NormalizeEmail(in.Email),
		Status: StatusInvited, Roles: []string{}, Theme: ThemeSystem, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	u.SetName(in.Name.Trimmed())
	if in.IsAdmin {
		u.Roles = []string{RoleAdmin}
	}
	if in.Password != nil {
		hash, err := s.passwords.Hash(*in.Password)
		if err != nil {
			return User{}, err
		}
		u.PasswordHash, u.Status, u.PasswordChangedAt = hash, StatusActive, &now
	}
	ev := audit.New(ctx, p.AuditActor(), audit.UserCreated, "", now)
	ev.Changes = []string{"email", "username", "name", "status"}
	if in.IsAdmin {
		ev.Changes = append(ev.Changes, "roles")
	}
	if in.Username != nil {
		u.Username = NormalizeUsername(*in.Username)
		out, err := s.repo.Insert(ctx, u, ev)
		return out, mapErr(err)
	}
	// Derived username: try the base, then numeric suffixes. The unique
	// index decides; a concurrent create taking a candidate only moves this
	// one to the next suffix.
	base := UsernameBase(in.Email)
	for n := 1; n <= maxUsernameAttempts; n++ {
		u.Username = UsernameCandidate(base, n)
		out, err := s.repo.Insert(ctx, u, ev)
		if !errors.Is(err, ErrUsernameTaken) {
			return out, mapErr(err)
		}
	}
	return User{}, mapErr(ErrUsernameTaken)
}

// maxUsernameAttempts bounds the numeric suffixes tried for a derived
// username before the create fails with usernameTaken.
const maxUsernameAttempts = 100

// UpdateInput is an administrative PATCH; nil fields are unchanged.
type UpdateInput struct {
	Name     NamePatch
	Username *string
	IsAdmin  *bool
	Status   *string
}

// Update changes a user's name, administrator role or status. Disabling a
// user revokes all of their sessions in the same transaction.
func (s *Service) Update(ctx context.Context, p access.Principal, id string, expectedVersion int64, in UpdateInput) (User, error) {
	if err := p.RequireAdmin(); err != nil {
		return User{}, err
	}
	if in.Username != nil {
		norm := NormalizeUsername(*in.Username)
		in.Username = &norm
	}
	var v validation.Collector
	v.OptionalString(validation.UserUsername, "username", in.Username)
	v.OptionalString(validation.UserStatus, "status", in.Status)
	if err := v.Err(); err != nil {
		return User{}, err
	}
	u, err := s.Get(ctx, p, id)
	if err != nil {
		return User{}, err
	}
	if u.Version != expectedVersion {
		return User{}, mapErr(ErrVersionConflict)
	}
	var changes []string
	revoke, guard := false, false
	if !in.Name.Empty() {
		// The resulting name is checked as a whole.
		n := in.Name.Apply(u.PersonName())
		n.Validate(&v, "")
		if err := v.Err(); err != nil {
			return User{}, err
		}
		if n != u.PersonName() {
			u.SetName(n)
			changes = append(changes, "name")
		}
	}
	if in.Username != nil && *in.Username != u.Username {
		u.Username = *in.Username
		changes = append(changes, "username")
	}
	if in.IsAdmin != nil && *in.IsAdmin != u.IsAdmin() {
		if *in.IsAdmin {
			u.Roles = append(slices.Clone(u.Roles), RoleAdmin)
		} else {
			if u.Id == p.Id {
				return User{}, fault.Conflict("cannotChangeOwnAdmin", "You cannot remove your own administrator role.")
			}
			u.Roles = slices.DeleteFunc(slices.Clone(u.Roles), func(r string) bool { return r == RoleAdmin })
			guard = true
		}
		changes = append(changes, "roles")
	}
	if in.Status != nil && *in.Status != u.Status {
		switch *in.Status {
		case StatusDisabled:
			if u.Id == p.Id {
				return User{}, fault.Conflict("cannotDisableSelf", "You cannot disable your own account.")
			}
			revoke, guard = true, guard || u.IsAdmin()
		case StatusActive:
			if u.PasswordHash == "" {
				return User{}, fault.Conflict("passwordRequired", "Set a password before activating this user.")
			}
		}
		u.Status = *in.Status
		changes = append(changes, "status")
	}
	if len(changes) == 0 {
		return u, nil
	}
	now := s.clock.Now()
	u.UpdatedAt = now
	action := audit.UserUpdated
	if revoke {
		action = audit.UserDisabled
	}
	ev := audit.New(ctx, p.AuditActor(), action, "user/"+u.Id, now)
	ev.Changes = changes
	out, err := s.repo.Apply(ctx, Change{User: u, ExpectedVersion: expectedVersion, BumpVersion: true,
		RevokeSessions: revoke, GuardLastAdmin: guard, Disabled: revoke, Event: &ev})
	return out, mapErr(err)
}

// SetPassword lets an administrator set a user's password. An invited user
// becomes active; every existing session of the user is revoked.
func (s *Service) SetPassword(ctx context.Context, p access.Principal, id, password string) error {
	if err := p.RequireAdmin(); err != nil {
		return err
	}
	var v validation.Collector
	v.String(validation.UserPassword, "password", password)
	if err := v.Err(); err != nil {
		return err
	}
	u, err := s.Get(ctx, p, id)
	if err != nil {
		return err
	}
	hash, err := s.passwords.Hash(password)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	changes := []string{"password"}
	u.PasswordHash, u.PasswordChangedAt, u.UpdatedAt = hash, &now, now
	if u.Status == StatusInvited {
		u.Status = StatusActive
		changes = append(changes, "status")
	}
	ev := audit.New(ctx, p.AuditActor(), audit.UserPasswordSet, "user/"+u.Id, now)
	ev.Changes = changes
	keep := ""
	if u.Id == p.Id {
		keep = p.SessionId
	}
	_, err = s.repo.Apply(ctx, Change{User: u, ExpectedVersion: u.Version, BumpVersion: true, RevokeSessions: true, KeepSessionId: keep, Event: &ev})
	return mapErr(err)
}

// ChangePassword changes the caller's own password after verifying the
// current one. Other sessions are revoked; the current one stays signed in.
func (s *Service) ChangePassword(ctx context.Context, p access.Principal, current, next string) error {
	if err := p.RequireUser(); err != nil {
		return err
	}
	var v validation.Collector
	v.String(validation.AuthPassword, "currentPassword", current)
	v.String(validation.UserPassword, "newPassword", next)
	if err := v.Err(); err != nil {
		return err
	}
	keys := []string{"pwchange:" + p.Id}
	if wait, err := s.throttle.Check(ctx, keys); err != nil {
		return err
	} else if wait > 0 {
		return throttled(wait)
	}
	u, err := s.repo.Get(ctx, p.WorkspaceId, p.Id)
	if err != nil {
		return mapErr(err)
	}
	if wait, err := s.throttle.Begin(ctx, keys); err != nil {
		return err
	} else if wait > 0 {
		return throttled(wait)
	}
	if err := s.passwords.Verify(current, u.PasswordHash); err != nil {
		return fault.Field("currentPassword", "incorrect", "The current password is incorrect.")
	}
	_ = s.throttle.Reset(ctx, keys)
	hash, err := s.passwords.Hash(next)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	u.PasswordHash, u.PasswordChangedAt, u.UpdatedAt = hash, &now, now
	ev := audit.New(ctx, p.AuditActor(), audit.UserPasswordChanged, "user/"+u.Id, now)
	ev.Changes = []string{"password"}
	_, err = s.repo.Apply(ctx, Change{User: u, Only: OnlyPassword, BumpVersion: true, RevokeSessions: true, KeepSessionId: p.SessionId, Event: &ev})
	return mapErr(err)
}

// ProfileInput is the caller's own profile PATCH; nil fields are unchanged.
type ProfileInput struct {
	Name NamePatch
}

// UpdateProfile changes the caller's own display name. It is versioned like
// an administrative edit (If-Match on the user version) and audited in the
// same transaction; it can never touch roles, status or the username.
func (s *Service) UpdateProfile(ctx context.Context, p access.Principal, expectedVersion int64, in ProfileInput) (User, error) {
	if err := p.RequireUser(); err != nil {
		return User{}, err
	}
	u, err := s.repo.Get(ctx, p.WorkspaceId, p.Id)
	if err != nil {
		return User{}, mapErr(err)
	}
	if u.Version != expectedVersion {
		return User{}, mapErr(ErrVersionConflict)
	}
	if in.Name.Empty() {
		return u, nil
	}
	n := in.Name.Apply(u.PersonName())
	var v validation.Collector
	n.Validate(&v, "")
	if err := v.Err(); err != nil {
		return User{}, err
	}
	if n == u.PersonName() {
		return u, nil
	}
	now := s.clock.Now()
	u.SetName(n)
	u.UpdatedAt = now
	ev := audit.New(ctx, p.AuditActor(), audit.UserProfileUpdated, "user/"+u.Id, now)
	ev.Changes = []string{"name"}
	out, err := s.repo.Apply(ctx, Change{User: u, Only: OnlyProfile, ExpectedVersion: expectedVersion, BumpVersion: true, Event: &ev})
	return out, mapErr(err)
}

// SetTheme stores the caller's theme preference.
func (s *Service) SetTheme(ctx context.Context, p access.Principal, theme string) (User, error) {
	if err := p.RequireUser(); err != nil {
		return User{}, err
	}
	var v validation.Collector
	v.String(validation.UserTheme, "theme", theme)
	if err := v.Err(); err != nil {
		return User{}, err
	}
	u, err := s.repo.Get(ctx, p.WorkspaceId, p.Id)
	if err != nil {
		return User{}, mapErr(err)
	}
	if u.Theme == theme {
		return u, nil
	}
	u.Theme = theme
	out, err := s.repo.Apply(ctx, Change{User: u, Only: OnlyTheme})
	return out, mapErr(err)
}

// PreferencesInput changes personal preferences; nil fields are unchanged.
type PreferencesInput struct {
	Theme         *string
	Signature     *string // empty clears
	WatcherEmails *bool   // false mutes watcher update emails
	QueueEmails   *bool   // false mutes new-ticket queue emails
}

// SetPreferences stores the caller's theme and email preferences.
func (s *Service) SetPreferences(ctx context.Context, p access.Principal, in PreferencesInput) (User, error) {
	if err := p.RequireUser(); err != nil {
		return User{}, err
	}
	var v validation.Collector
	if in.Theme != nil {
		v.String(validation.UserTheme, "theme", *in.Theme)
	}
	if in.Signature != nil {
		v.String(validation.UserSignature, "signature", strings.TrimSpace(*in.Signature))
	}
	if in.Theme == nil && in.Signature == nil && in.WatcherEmails == nil && in.QueueEmails == nil {
		v.Add("theme", validation.CodeRequired, "Send at least one preference.")
	}
	if err := v.Err(); err != nil {
		return User{}, err
	}
	u, err := s.repo.Get(ctx, p.WorkspaceId, p.Id)
	if err != nil {
		return User{}, mapErr(err)
	}
	before := u
	if in.Theme != nil {
		u.Theme = *in.Theme
	}
	if in.Signature != nil {
		u.Signature = strings.TrimSpace(*in.Signature)
	}
	if in.WatcherEmails != nil {
		u.EmailPrefs.MuteWatcherUpdates = !*in.WatcherEmails
	}
	if in.QueueEmails != nil {
		u.EmailPrefs.MuteQueueUpdates = !*in.QueueEmails
	}
	if u.Theme == before.Theme && u.EmailPrefs == before.EmailPrefs && u.Signature == before.Signature {
		return u, nil
	}
	out, err := s.repo.Apply(ctx, Change{User: u, Only: OnlyPreferences})
	return out, mapErr(err)
}

func throttled(wait time.Duration) error {
	return fault.RateLimited("tooManyAttempts", "Too many attempts. Try again later.", wait)
}
