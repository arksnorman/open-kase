package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// ErrNotInitialized means no settings record exists (run init).
var ErrNotInitialized = errors.New("settings: installation is not initialized")

// ErrVersionConflict means the stored version differs from the expected one.
var ErrVersionConflict = errors.New("settings: version conflict")

// Record is a stored settings version.
type Record struct {
	WorkspaceId string
	Version     int64
	Settings    Settings
	UpdatedAt   time.Time
}

// Repository persists settings. Replace must atomically check the expected
// version, write the new record and append the audit event.
type Repository interface {
	Load(ctx context.Context) (Record, error)
	Replace(ctx context.Context, expectedVersion int64, next Record, event audit.Event) error
}

// Sealer encrypts secret settings values with the master key.
type Sealer interface {
	Seal(plaintext []byte, context string) (string, error)
}

// Snapshot is an immutable, validated view of one settings version.
type Snapshot struct {
	record Record
}

// Version returns the configVersion this snapshot was built from.
func (s *Snapshot) Version() int64 { return s.record.Version }

// WorkspaceID returns the owning workspace.
func (s *Snapshot) WorkspaceID() string { return s.record.WorkspaceId }

// Product returns product settings.
func (s *Snapshot) Product() ProductSettings { return s.record.Settings.Product }

// Auth returns authentication settings.
func (s *Snapshot) Auth() AuthSettings { return s.record.Settings.Auth }

// Files returns file limit settings.
func (s *Snapshot) Files() FileSettings { return s.record.Settings.Files }

// Notifications returns the requester email settings.
func (s *Snapshot) Notifications() NotificationSettings { return s.record.Settings.Notifications }

// SMTP returns SMTP settings including the sealed password; only the mail
// adapter should receive this view.
func (s *Snapshot) SMTP() SMTPSettings { return s.record.Settings.SMTP }

// Redacted returns the administrator-safe representation.
func (s *Snapshot) Redacted() Redacted {
	st := s.record.Settings
	return Redacted{
		Version:   s.record.Version,
		UpdatedAt: s.record.UpdatedAt,
		Product:   st.Product,
		Auth:      st.Auth,
		Files:     st.Files,
		SMTP: RedactedSMTP{
			Enabled: st.SMTP.Enabled, Host: st.SMTP.Host, Port: st.SMTP.Port, Username: st.SMTP.Username,
			PasswordConfigured: st.SMTP.PasswordCiphertext != "",
			FromAddress:        st.SMTP.FromAddress, FromName: st.SMTP.FromName, TLSMode: st.SMTP.TLSMode,
			CACertificate: st.SMTP.CACertificate, TLSSkipVerify: st.SMTP.TLSSkipVerify,
		},
		Notifications: st.Notifications,
	}
}

// Service loads, caches and updates settings.
type Service struct {
	repo    Repository
	sealer  Sealer
	clock   clock.Clock
	current atomic.Pointer[Snapshot]
}

// NewService constructs a settings service.
func NewService(repo Repository, sealer Sealer, clk clock.Clock) *Service {
	return &Service{repo: repo, sealer: sealer, clock: clk}
}

// Current returns the last loaded snapshot, or nil before the first load.
func (s *Service) Current() *Snapshot { return s.current.Load() }

// Refresh loads the stored settings and swaps the snapshot when the version
// changed. Invalid stored settings are rejected and the previous snapshot is
// retained.
func (s *Service) Refresh(ctx context.Context) (*Snapshot, error) {
	rec, err := s.repo.Load(ctx)
	if err != nil {
		return s.Current(), err
	}
	if cur := s.Current(); cur != nil && cur.Version() == rec.Version {
		return cur, nil
	}
	if err := rec.Settings.Validate(); err != nil {
		return s.Current(), fmt.Errorf("stored settings version %d are invalid: %w", rec.Version, err)
	}
	snap := &Snapshot{record: rec}
	s.current.Store(snap)
	return snap, nil
}

// Update describes a settings change. SMTPPassword nil keeps the existing
// password; a pointer to "" clears it.
type Update struct {
	Actor           audit.Actor
	RequestId       string
	ExpectedVersion int64
	Settings        Settings
	// Mutate, when set, edits a copy of the stored settings instead of
	// replacing them with Settings (partial updates such as SMTP only).
	Mutate       func(*Settings)
	SMTPPassword *string
}

// Update validates and stores a new settings version with an audit event.
func (s *Service) Update(ctx context.Context, u Update) (*Snapshot, error) {
	if u.Actor.Type == "" {
		return nil, errors.New("settings update requires an actor")
	}
	stored, err := s.repo.Load(ctx)
	if err != nil {
		return nil, err
	}
	if stored.Version != u.ExpectedVersion {
		return nil, fault.Conflict("versionConflict", "Settings were changed by someone else. Reload and try again.")
	}
	next := u.Settings
	if u.Mutate != nil {
		next = stored.Settings
		u.Mutate(&next)
	}
	next.SMTP.PasswordCiphertext = stored.Settings.SMTP.PasswordCiphertext
	// A stored password is only reused for the server it was entered for:
	// otherwise an administrator session (or a stolen one) could point SMTP
	// at its own server and receive the password with the next send.
	if u.SMTPPassword == nil && stored.Settings.SMTP.PasswordCiphertext != "" && smtpDestinationChanged(stored.Settings.SMTP, next.SMTP) {
		return nil, fault.Field("smtp.password", validation.CodeRequired,
			"Enter the password again when changing the server, port, user name or TLS mode (or send an empty password to remove it).")
	}
	if u.SMTPPassword != nil {
		next.SMTP.PasswordCiphertext = ""
		if *u.SMTPPassword != "" {
			sealed, err := s.sealer.Seal([]byte(*u.SMTPPassword), SMTPPasswordContext)
			if err != nil {
				return nil, fmt.Errorf("seal smtp password: %w", err)
			}
			next.SMTP.PasswordCiphertext = sealed
		}
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	changes := Diff(stored.Settings, next)
	if len(changes) == 0 {
		return s.Refresh(ctx)
	}
	now := s.clock.Now()
	rec := Record{WorkspaceId: stored.WorkspaceId, Version: stored.Version + 1, Settings: next, UpdatedAt: now}
	event := audit.Event{
		WorkspaceId: stored.WorkspaceId, Action: audit.SettingsUpdated, Actor: u.Actor, RequestId: u.RequestId,
		Target: "settings/installation", Changes: changes, Outcome: "success", CreatedAt: now,
	}
	if err := s.repo.Replace(ctx, u.ExpectedVersion, rec, event); err != nil {
		if errors.Is(err, ErrVersionConflict) {
			return nil, fault.Conflict("versionConflict", "Settings were changed by someone else. Reload and try again.")
		}
		return nil, err
	}
	return s.Refresh(ctx)
}

// smtpDestinationChanged reports whether the stored SMTP password would be
// sent somewhere (or in a way) it was not entered for.
func smtpDestinationChanged(a, b SMTPSettings) bool {
	return !strings.EqualFold(strings.TrimSpace(a.Host), strings.TrimSpace(b.Host)) || a.Port != b.Port ||
		a.TLSMode != b.TLSMode || a.Username != b.Username ||
		// Weakening verification sends the password to a peer that is no longer authenticated the same way.
		(!a.TLSSkipVerify && b.TLSSkipVerify) ||
		(strings.TrimSpace(b.CACertificate) != "" && strings.TrimSpace(a.CACertificate) != strings.TrimSpace(b.CACertificate))
}
