package settings

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
)

func TestDefaultsAreValid(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestValidateCrossFieldRules(t *testing.T) {
	s := Defaults()
	s.SMTP.Enabled = true
	err := s.Validate()
	fe := fault.As(err)
	if fe.Kind != fault.KindValidation || len(fe.Fields) != 3 {
		t.Fatalf("expected 3 field errors, got %v", err)
	}
	s = Defaults()
	s.SMTP.Username = "mailer"
	s.SMTP.TLSMode = "none"
	if s.Validate() == nil {
		t.Fatal("authenticated plaintext SMTP accepted")
	}
}

func TestRequesterLinkBaseURL(t *testing.T) {
	s := Defaults()
	s.Product.BaseURL = "https://support.example.com/"
	for raw, ok := range map[string]bool{
		"":                                   true,
		"https://portal.example.org":         true,
		"http://portal.example.org/req/":     true,
		"https://support.example.com":        false, // the agent UI
		"https://SUPPORT.example.com/portal": false,
		"https://support.example.com.evil":   true,
		"mailto:x@example.org":               false,
		"https://portal.example.org/?a=1":    false,
		"https://portal.example.org/a b":     false,
	} {
		s.Notifications.RequesterLinkBaseURL = raw
		if err := s.Validate(); (err == nil) != ok {
			t.Errorf("%q: err = %v", raw, err)
		}
	}
	n := NotificationSettings{RequesterLinkBaseURL: "https://portal.example.org/req/"}
	if got := n.RequesterTicketURL(4); got != "https://portal.example.org/req/4" {
		t.Errorf("url = %q", got)
	}
	if got := (NotificationSettings{}).RequesterTicketURL(4); got != "" {
		t.Errorf("unset url = %q", got)
	}
}

type memRepo struct {
	rec    Record
	events []audit.Event
}

func (m *memRepo) Load(context.Context) (Record, error) {
	if m.rec.Version == 0 {
		return Record{}, ErrNotInitialized
	}
	return m.rec, nil
}

func (m *memRepo) Replace(_ context.Context, expected int64, next Record, ev audit.Event) error {
	if m.rec.Version != expected {
		return ErrVersionConflict
	}
	m.rec = next
	m.events = append(m.events, ev)
	return nil
}

type fakeSealer struct{}

func (fakeSealer) Seal(p []byte, c string) (string, error) { return "sealed(" + c + ")", nil }

func TestServiceUpdateVersionsRedactsAndAudits(t *testing.T) {
	repo := &memRepo{rec: Record{WorkspaceId: "w1", Version: 1, Settings: Defaults()}}
	svc := NewService(repo, fakeSealer{}, clock.NewManual(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
	if _, err := svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	next := Defaults()
	next.Product.BaseURL = "https://support.example.com"
	next.SMTP.Host = "smtp.example.com"
	next.SMTP.Username = "mailer"
	pw := "s3cret-smtp"
	snap, err := svc.Update(context.Background(), Update{Actor: audit.Operator(audit.SystemCLI, "test"), ExpectedVersion: 1, Settings: next, SMTPPassword: &pw})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version() != 2 || svc.Current().Version() != 2 {
		t.Fatalf("version = %d", snap.Version())
	}
	red := snap.Redacted()
	if !red.SMTP.PasswordConfigured || red.Product.BaseURL != "https://support.example.com" {
		t.Fatalf("redacted = %+v", red)
	}
	if len(repo.events) != 1 || !slices.Contains(repo.events[0].Changes, "smtp.password") || repo.events[0].Action != "settings.updated" {
		t.Fatalf("audit = %+v", repo.events)
	}

	// Stale expected version is a conflict.
	_, err = svc.Update(context.Background(), Update{Actor: audit.Operator(audit.SystemCLI, "test"), ExpectedVersion: 1, Settings: next})
	if fe := fault.As(err); fe.Kind != fault.KindConflict {
		t.Fatalf("stale update: %v", err)
	}

	// Omitting the password keeps it.
	next.Product.Name = "Help Desk"
	snap, err = svc.Update(context.Background(), Update{Actor: audit.Operator(audit.SystemCLI, "test"), ExpectedVersion: 2, Settings: next})
	if err != nil || !snap.Redacted().SMTP.PasswordConfigured || snap.Product().Name != "Help Desk" {
		t.Fatalf("keep password: %v %+v", err, snap)
	}

	// Invalid settings are rejected without a new version.
	next.SMTP.Port = 0
	if _, err := svc.Update(context.Background(), Update{Actor: audit.Operator(audit.SystemCLI, "test"), ExpectedVersion: 3, Settings: next}); fault.As(err).Kind != fault.KindValidation {
		t.Fatalf("invalid update: %v", err)
	}
	if repo.rec.Version != 3 {
		t.Fatalf("version advanced on invalid update: %d", repo.rec.Version)
	}
}

func TestRefreshRejectsInvalidStoredSettings(t *testing.T) {
	repo := &memRepo{rec: Record{Version: 1, Settings: Defaults()}}
	svc := NewService(repo, fakeSealer{}, clock.System{})
	svc.Refresh(context.Background())
	bad := Defaults()
	bad.SMTP.TLSMode = "ssl3"
	repo.rec = Record{Version: 2, Settings: bad}
	if _, err := svc.Refresh(context.Background()); err == nil {
		t.Fatal("invalid stored settings accepted")
	}
	if svc.Current().Version() != 1 {
		t.Fatal("previous valid snapshot not retained")
	}
	empty := NewService(&memRepo{}, fakeSealer{}, clock.System{})
	if _, err := empty.Refresh(context.Background()); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("uninitialized: %v", err)
	}
}
