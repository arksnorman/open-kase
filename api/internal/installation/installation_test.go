package installation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
)

type memRepo struct {
	state State
	seeds []Seed
}

func (m *memRepo) State(context.Context) (State, error) { return m.state, nil }

func (m *memRepo) Seed(_ context.Context, s Seed) (Result, error) {
	m.seeds = append(m.seeds, s)
	r := Result{WorkspaceId: "w1", WorkspaceCreated: !m.state.Initialized, AdminCreated: s.Admin != nil && !m.state.AdminExists}
	m.state = State{Initialized: true, WorkspaceId: "w1", WorkspaceName: s.WorkspaceName, KeyCheck: s.KeyCheck, AdminExists: m.state.AdminExists || s.Admin != nil}
	return r, nil
}

type fakeKeys struct{ check string }

func (f fakeKeys) NewKeyCheck() (string, error) { return f.check, nil }
func (f fakeKeys) VerifyKeyCheck(c string) error {
	if c != f.check {
		return errors.New("bad key")
	}
	return nil
}

func hasher(p string) (string, error) { return "hash:" + p, nil }

func newService(repo *memRepo, check string) *Service {
	return NewService(repo, fakeKeys{check: check}, hasher, clock.NewManual(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
}

func TestInitializeFreshThenRepeat(t *testing.T) {
	repo := &memRepo{}
	svc := newService(repo, "kc1")
	in := Input{WorkspaceName: "Acme Support", AdminEmail: "Admin@Example.com", AdminName: users.PersonName{Given: "Ada", Family: "Admin"}, AdminPassword: "a long enough password"}
	res, err := svc.Initialize(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.WorkspaceCreated || !res.AdminCreated {
		t.Fatalf("result = %+v", res)
	}
	seed := repo.seeds[0]
	if seed.KeyCheck != "kc1" || seed.Admin.EmailNormalized != "admin@example.com" || seed.Admin.PasswordHash != "hash:a long enough password" {
		t.Fatalf("seed = %+v", seed)
	}
	if len(seed.Statuses) != 5 || len(seed.RequestTypes) != 4 {
		t.Fatalf("defaults missing: %+v", seed)
	}

	// Repeat without admin details: no admin seed, workspace name preserved.
	res, err = svc.Initialize(context.Background(), Input{})
	if err != nil {
		t.Fatalf("repeat init: %v", err)
	}
	if repo.seeds[1].Admin != nil || repo.seeds[1].WorkspaceName != "Acme Support" || res.WorkspaceCreated {
		t.Fatalf("repeat seed = %+v", repo.seeds[1])
	}
}

func TestInitializeRejectsWrongKey(t *testing.T) {
	repo := &memRepo{state: State{Initialized: true, KeyCheck: "kc1", AdminExists: true}}
	_, err := newService(repo, "other").Initialize(context.Background(), Input{})
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestInitializeValidatesAdmin(t *testing.T) {
	_, err := newService(&memRepo{}, "kc").Initialize(context.Background(), Input{WorkspaceName: "W", AdminEmail: "bad", AdminName: users.PersonName{Given: "A", Family: "Admin"}, AdminPassword: "short"})
	fe := fault.As(err)
	if fe.Kind != fault.KindValidation || len(fe.Fields) != 2 {
		t.Fatalf("expected email and password errors, got %v", err)
	}
}
