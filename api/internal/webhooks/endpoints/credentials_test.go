package endpoints

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

type fakeKeys struct{}

func (fakeKeys) Seal(p []byte, c string) (string, error) {
	return "sealed:" + c + ":" + string(p), nil
}
func (fakeKeys) Open(s, c string) ([]byte, error) {
	return []byte(strings.TrimPrefix(s, "sealed:"+c+":")), nil
}

type fakeRepo struct{ e Endpoint }

func (r *fakeRepo) NewID() string { return "0123456789abcdef01234567" }
func (r *fakeRepo) Create(_ context.Context, e Endpoint, _ string, _ audit.Event) (Endpoint, error) {
	r.e = e
	return e, nil
}
func (r *fakeRepo) List(context.Context, string, *pagination.Cursor, int) ([]Endpoint, error) {
	return []Endpoint{r.e}, nil
}
func (r *fakeRepo) Get(context.Context, string, string) (Endpoint, error) { return r.e, nil }
func (r *fakeRepo) Update(_ context.Context, e Endpoint, _ string, _ int64, _ audit.Event) (Endpoint, error) {
	e.Version++
	r.e = e
	return e, nil
}
func (r *fakeRepo) Delete(context.Context, string, string, time.Time, audit.Event) error { return nil }
func (r *fakeRepo) PrincipalActive(context.Context, string) (bool, error)                { return true, nil }
func (r *fakeRepo) Stats(context.Context, string, []string, time.Time) (map[string]Stats, error) {
	return nil, nil
}

type fakeDir struct{}

func (fakeDir) TheProject(context.Context, string) (access.ProjectRecord, error) {
	return access.ProjectRecord{Id: "p1", WorkspaceId: "ws"}, nil
}
func (fakeDir) UserGroups(context.Context, string, string) ([]string, error) { return nil, nil }

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func str(s string) *string { return &s }

func setup(t *testing.T) (*Service, *access.ProjectScope, *fakeRepo) {
	t.Helper()
	repo := &fakeRepo{}
	svc := NewService(Deps{Repo: repo, Keys: fakeKeys{}, Clock: fixedClock{},
		Policy: func() webhooks.Policy { return webhooks.Policy{} }})
	scope, err := access.NewService(fakeDir{}).Scope(context.Background(),
		access.Principal{Kind: access.KindUser, Id: "u1", WorkspaceId: "ws", Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	types := []string{"ticket.created"}
	if _, err := svc.Create(context.Background(), scope, Input{
		Name: str("hook"), URL: str("https://one.example.com/hook"), EventTypes: &types,
		Auth:    &AuthInput{Type: AuthBasic, Username: str("u"), Password: str("pw")},
		Headers: &[]HeaderInput{{Name: "X-Key", Value: str("secret")}},
	}); err != nil {
		t.Fatalf("%v %+v", err, fault.As(err).Fields)
	}
	return svc, scope, repo
}

func update(svc *Service, scope *access.ProjectScope, repo *fakeRepo, in Input) error {
	_, err := svc.Update(context.Background(), scope, repo.e.Id, repo.e.Version, in)
	return err
}

func TestUpdateDestinationNeedsCredentialsAgain(t *testing.T) {
	svc, scope, repo := setup(t)

	// Path-only change on the same host: no re-entry.
	if err := update(svc, scope, repo, Input{URL: str("https://one.example.com/other")}); err != nil {
		t.Fatalf("path-only change: %v", err)
	}
	// New host with the stored password: rejected.
	err := update(svc, scope, repo, Input{URL: str("https://two.example.com/hook")})
	fe := fault.As(err)
	if fe == nil || len(fe.Fields) == 0 || fe.Fields[0].Field != "auth" || fe.Fields[0].Code != "required" {
		t.Fatalf("host change without credentials: %v", err)
	}
	if repo.e.URL != "https://one.example.com/other" {
		t.Fatalf("URL changed despite the rejection: %s", repo.e.URL)
	}
	// A different port counts too.
	if fe := fault.As(update(svc, scope, repo, Input{URL: str("https://one.example.com:8443/hook")})); fe == nil {
		t.Fatal("port change accepted")
	}
	// Password re-supplied but a secret header kept: still rejected.
	err = update(svc, scope, repo, Input{URL: str("https://two.example.com/hook"),
		Auth: &AuthInput{Type: AuthBasic, Password: str("new")}})
	if fe := fault.As(err); fe == nil || fe.Fields[0].Field != "headers[0].value" {
		t.Fatalf("kept header on new host: %v", err)
	}
	// Everything re-supplied: accepted.
	err = update(svc, scope, repo, Input{URL: str("https://two.example.com/hook"),
		Auth:    &AuthInput{Type: AuthBasic, Password: str("new")},
		Headers: &[]HeaderInput{{Name: "X-Key", Value: str("new")}}})
	if err != nil {
		t.Fatalf("re-entered credentials: %v", err)
	}
}

func TestUpdateDestinationClearingAuthNeedsNoReentry(t *testing.T) {
	svc, scope, repo := setup(t)
	err := update(svc, scope, repo, Input{URL: str("https://two.example.com/hook"),
		Auth: &AuthInput{Type: AuthNone}, Headers: &[]HeaderInput{}})
	if err != nil {
		t.Fatalf("clearing auth and headers with a new host: %v", err)
	}
}

func TestDestinationChanged(t *testing.T) {
	ca := func(s string) Endpoint { return Endpoint{URL: "https://a.example.com", TLS: TLS{CACertificate: s}} }
	if destinationChanged(ca("x"), ca("")) {
		t.Error("removing the CA is not a destination change")
	}
	if !destinationChanged(ca(""), ca("x")) || !destinationChanged(ca("x"), ca("y")) {
		t.Error("a new CA is a destination change")
	}
	a, b := Endpoint{URL: "https://A.example.com/x"}, Endpoint{URL: "https://a.example.com:443/y"}
	if destinationChanged(a, b) {
		t.Error("case and the default port must not count")
	}
}
