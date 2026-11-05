package mongo

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/installation"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/secrets"
)

func migrate(t *testing.T, s *Store) {
	t.Helper()
	if err := s.EnsureSchema(testContext(t)); err != nil {
		t.Fatalf("schema: %v", err)
	}
}

func TestEnsureSchemaIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := testContext(t)
	migrate(t, s)
	migrate(t, s) // a second run on a database that has the schema is a no-op
	names, err := s.db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range schemaSpecs() {
		if !slices.Contains(names, spec.name) {
			t.Errorf("missing collection %s", spec.name)
			continue
		}
		cur, err := s.db.Collection(spec.name).Indexes().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []struct {
			Name string `bson:"name"`
		}
		if err := cur.All(ctx, &got); err != nil {
			t.Fatal(err)
		}
		for _, ix := range spec.indexes {
			var o options.IndexOptions
			for _, f := range ix.Options.Opts {
				_ = f(&o)
			}
			want := *o.Name
			if !slices.ContainsFunc(got, func(g struct {
				Name string `bson:"name"`
			}) bool {
				return g.Name == want
			}) {
				t.Errorf("missing index %s.%s", spec.name, want)
			}
		}
	}
}

func TestValidatorsAndUniqueIndexes(t *testing.T) {
	s := testStore(t)
	ctx := testContext(t)
	migrate(t, s)
	ws := bson.NewObjectID()
	now := time.Now().UTC()
	user := func(email, status string) bson.D {
		return bson.D{{Key: "workspaceId", Value: ws}, {Key: "email", Value: email}, {Key: "emailNormalized", Value: strings.ToLower(email)},
			{Key: "username", Value: "u" + bson.NewObjectID().Hex()}, {Key: "displayName", Value: "U"}, {Key: "givenName", Value: "U"}, {Key: "status", Value: status}, {Key: "roles", Value: bson.A{}},
			{Key: "createdAt", Value: now}, {Key: "updatedAt", Value: now}, {Key: "version", Value: int64(1)}}
	}
	if _, err := s.col(colUsers).InsertOne(ctx, user("a@example.com", "active")); err != nil {
		t.Fatalf("valid user rejected: %v", err)
	}
	if _, err := s.col(colUsers).InsertOne(ctx, user("A@example.com", "active")); err == nil {
		t.Fatal("duplicate normalized email accepted")
	}
	if _, err := s.col(colUsers).InsertOne(ctx, user("b@example.com", "superuser")); err == nil {
		t.Fatal("invalid user status accepted by validator")
	}
	project := func(key string) bson.D {
		return bson.D{{Key: "workspaceId", Value: ws}, {Key: "key", Value: key}, {Key: "name", Value: "P"}, {Key: "status", Value: "active"},
			{Key: "createdAt", Value: now}, {Key: "updatedAt", Value: now}, {Key: "version", Value: int64(1)}}
	}
	if _, err := s.col(colProjects).InsertOne(ctx, project("MAIL")); err != nil {
		t.Fatalf("valid project rejected: %v", err)
	}
	if _, err := s.col(colProjects).InsertOne(ctx, project("MAIL")); err == nil {
		t.Fatal("duplicate project key accepted")
	}
	if _, err := s.col(colProjects).InsertOne(ctx, project("mail")); err == nil {
		t.Fatal("lowercase project key accepted by validator")
	}
	status := bson.D{{Key: "workspaceId", Value: ws}, {Key: "projectId", Value: nil}, {Key: "code", Value: "x"}, {Key: "label", Value: "X"},
		{Key: "category", Value: "closed"}, {Key: "builtin", Value: false}, {Key: "active", Value: true}, {Key: "order", Value: int64(1)}, {Key: "createdAt", Value: now}}
	if _, err := s.col(colStatuses).InsertOne(ctx, status); err == nil {
		t.Fatal("unknown status category accepted")
	}
}

var fastArgon = auth.Argon2Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func newInstallService(t *testing.T, s *Store, key byte) *installation.Service {
	t.Helper()
	kr, err := secrets.NewKeyring(bytes.Repeat([]byte{key}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return installation.NewService(NewInstallationRepository(s), kr,
		func(pw string) (string, error) { return auth.HashPassword(pw, fastArgon) }, clock.System{})
}

func counts(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, c := range []string{colWorkspaces, colSettings, colUsers, colStatuses, colRequestTypes, colAuditEvents, colProjects, colGroups} {
		n, err := s.col(c).CountDocuments(testContext(t), bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		out[c] = n
	}
	return out
}

func TestInstallationIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := testContext(t)
	migrate(t, s)
	svc := newInstallService(t, s, 1)
	in := installation.Input{WorkspaceName: "Acme", AdminEmail: "Admin@Example.com", AdminName: users.PersonName{Given: "Ada", Family: "Admin"}, AdminPassword: "correct horse battery", Actor: audit.Operator(audit.SystemCLI, "test")}

	res, err := svc.Initialize(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.WorkspaceCreated || !res.SettingsCreated || !res.AdminCreated || res.StatusesCreated != 5 || res.RequestTypesCreated != 4 ||
		!res.ProjectCreated {
		t.Fatalf("first init: %+v", res)
	}
	first := counts(t, s)
	// Audit: installation, project, Triage group, administrator.
	want := map[string]int64{colWorkspaces: 1, colSettings: 1, colUsers: 1, colStatuses: 5, colRequestTypes: 4, colAuditEvents: 4,
		colProjects: 1, colGroups: 1}
	for k, v := range want {
		if first[k] != v {
			t.Errorf("%s = %d, want %d", k, first[k], v)
		}
	}

	var u struct {
		EmailNormalized string   `bson:"emailNormalized"`
		PasswordHash    string   `bson:"passwordHash"`
		Roles           []string `bson:"roles"`
	}
	if err := s.col(colUsers).FindOne(ctx, bson.D{}).Decode(&u); err != nil {
		t.Fatal(err)
	}
	if u.EmailNormalized != "admin@example.com" || !strings.HasPrefix(u.PasswordHash, "$argon2id$") || !slices.Equal(u.Roles, []string{"admin"}) {
		t.Fatalf("admin = %+v", u)
	}
	if _, err := auth.VerifyPassword("correct horse battery", u.PasswordHash, fastArgon); err != nil {
		t.Fatalf("stored hash does not verify: %v", err)
	}

	// Repeat: nothing duplicated, a second admin is not created.
	res, err = svc.Initialize(ctx, installation.Input{AdminEmail: "other@example.com", AdminName: users.PersonName{Given: "O", Family: "Other"}, AdminPassword: "another long password"})
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkspaceCreated || res.AdminCreated || res.StatusesCreated != 0 || res.RequestTypesCreated != 0 || res.SettingsCreated ||
		res.ProjectCreated {
		t.Fatalf("repeat init created records: %+v", res)
	}
	if again := counts(t, s); !mapsEqual(again, first) {
		t.Fatalf("counts changed: %v -> %v", first, again)
	}

	// A different master key is rejected.
	if _, err := newInstallService(t, s, 2).Initialize(ctx, installation.Input{}); !errors.Is(err, installation.ErrKeyMismatch) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestConcurrentInitializationCreatesOneInstallation(t *testing.T) {
	s := testStore(t)
	ctx := testContext(t)
	migrate(t, s)
	svc := newInstallService(t, s, 1)

	var wg sync.WaitGroup
	errs := make([]error, 6)
	admins := make([]bool, 6)
	for i := range 6 {
		wg.Go(func() {
			res, err := svc.Initialize(ctx, installation.Input{WorkspaceName: "Acme",
				AdminEmail: "admin" + string(rune('a'+i)) + "@example.com", AdminName: users.PersonName{Given: "A", Family: "Admin"}, AdminPassword: "correct horse battery"})
			errs[i], admins[i] = err, res.AdminCreated
		})
	}
	wg.Wait()
	created := 0
	for i := range errs {
		if errs[i] != nil {
			t.Errorf("init %d: %v", i, errs[i])
		}
		if admins[i] {
			created++
		}
	}
	c := counts(t, s)
	if c[colWorkspaces] != 1 || c[colUsers] != 1 || c[colStatuses] != 5 || c[colRequestTypes] != 4 || c[colSettings] != 1 || created != 1 {
		t.Fatalf("concurrent init: counts=%v adminsCreated=%d", c, created)
	}
}

func TestSettingsUpdateIsVersionedAuditedAndEncrypted(t *testing.T) {
	s := testStore(t)
	ctx := testContext(t)
	migrate(t, s)
	if _, err := newInstallService(t, s, 1).Initialize(ctx, installation.Input{WorkspaceName: "Acme",
		AdminEmail: "a@example.com", AdminName: users.PersonName{Given: "A", Family: "Admin"}, AdminPassword: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	kr, _ := secrets.NewKeyring(bytes.Repeat([]byte{1}, secrets.KeySize))
	repo := NewSettingsRepository(s)
	svc := settings.NewService(repo, kr, clock.System{})
	snap, err := svc.Refresh(ctx)
	if err != nil || snap.Version() != 1 {
		t.Fatalf("initial snapshot: %v %v", snap, err)
	}

	next := settings.Defaults()
	next.Product.BaseURL = "https://support.example.com"
	next.SMTP.Host = "smtp.example.com"
	next.SMTP.Username = "mailer"
	pw := "smtp-secret-value"
	snap, err = svc.Update(ctx, settings.Update{Actor: audit.Operator(audit.SystemCLI, "test"), ExpectedVersion: 1, Settings: next, SMTPPassword: &pw})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version() != 2 || !snap.Redacted().SMTP.PasswordConfigured {
		t.Fatalf("snapshot: %+v", snap.Redacted())
	}

	var raw settingsDoc
	if err := s.col(colSettings).FindOne(ctx, bson.D{}).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	stored := raw.Data.SMTP.PasswordCiphertext
	if strings.Contains(stored, pw) {
		t.Fatal("SMTP password stored in plaintext")
	}
	if plain, err := kr.Open(stored, settings.SMTPPasswordContext); err != nil || string(plain) != pw {
		t.Fatalf("stored ciphertext does not decrypt: %v", err)
	}

	var ev struct {
		Action  string   `bson:"action"`
		Changes []string `bson:"changes"`
	}
	if err := s.col(colAuditEvents).FindOne(ctx, bson.D{{Key: "action", Value: "settings.updated"}}).Decode(&ev); err != nil {
		t.Fatalf("audit event: %v", err)
	}
	if !slices.Contains(ev.Changes, "smtp.password") || !slices.Contains(ev.Changes, "product.baseUrl") {
		t.Fatalf("audit changes: %v", ev.Changes)
	}
	auditRaw, _ := s.col(colAuditEvents).FindOne(ctx, bson.D{{Key: "action", Value: "settings.updated"}}).Raw()
	if strings.Contains(auditRaw.String(), pw) {
		t.Fatal("audit event contains the secret")
	}

	// Stale version conflicts at the repository level too.
	err = repo.Replace(ctx, 1, settings.Record{WorkspaceId: snap.WorkspaceID(), Version: 2, Settings: next}, audit.Event{Action: "settings.updated", Actor: audit.Operator(audit.SystemCLI, "x"), CreatedAt: time.Now()})
	if !errors.Is(err, settings.ErrVersionConflict) {
		t.Fatalf("stale replace: %v", err)
	}
	if _, err := svc.Update(ctx, settings.Update{Actor: audit.Operator(audit.SystemCLI, "test"), ExpectedVersion: 1, Settings: next}); fault.As(err).Kind != fault.KindConflict {
		t.Fatalf("stale update: %v", err)
	}
}

func mapsEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
