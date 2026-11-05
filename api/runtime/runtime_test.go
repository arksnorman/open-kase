package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/installation"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/logger"
	"github.com/arksnorman/open-kase/api/package/secrets"
)

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestExecuteUsageAndVersion(t *testing.T) {
	var out, errb bytes.Buffer
	stdio := IO{In: devNull(t), Out: &out, Err: &errb}
	if code := Execute(context.Background(), nil, stdio); code != ExitUsage || !strings.Contains(out.String(), "Commands:") {
		t.Fatalf("no args: %d %s", code, out.String())
	}
	if code := Execute(context.Background(), []string{"bogus"}, stdio); code != ExitUsage || !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("unknown: %d %s", code, errb.String())
	}
	out.Reset()
	if code := Execute(context.Background(), []string{"version", "--json"}, stdio); code != ExitOK {
		t.Fatalf("version: %d", code)
	}
	var info map[string]string
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || info["product"] != "open-kase" || info["version"] == "" {
		t.Fatalf("version output %q: %v", out.String(), err)
	}
	errb.Reset()
	if code := Execute(context.Background(), []string{"init", "--help"}, stdio); code != ExitOK || !strings.Contains(errb.String(), "-password-stdin") {
		t.Fatalf("init --help: %d %s", code, errb.String())
	}
}

func TestBootstrapValidation(t *testing.T) {
	b := Bootstrap{Env: "staging", LogLevel: "info", MasterKeyFile: "/k"}
	err := b.validate(true)
	if err == nil || !strings.Contains(err.Error(), "APP_ENV") || !strings.Contains(err.Error(), "MONGODB_URI") || !strings.Contains(err.Error(), "REDIS_URI") {
		t.Fatalf("validate: %v", err)
	}
	t.Setenv("APP_ENV", "development")
	t.Setenv("MONGODB_URI", "mongodb://db/x?replicaSet=rs0")
	t.Setenv("OPEN_KASE_HTTP_ADDR", "0.0.0.0:9000")
	b = bootstrapFromEnv()
	if b.Env != "development" || b.HTTPAddr != "0.0.0.0:9000" || b.validate(false) != nil {
		t.Fatalf("from env: %+v %v", b, b.validate(false))
	}
}

// testMongoURI returns a URI for a fresh database dropped after the test.
func testMongoURI(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("OPEN_KASE_TEST_MONGO_URI")
	if raw == "" {
		t.Skip("set OPEN_KASE_TEST_MONGO_URI to run MongoDB integration tests")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/kase_test_" + bson.NewObjectID().Hex()
	uri := u.String()
	t.Cleanup(func() {
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = client.Database(strings.TrimPrefix(u.Path, "/")).Drop(ctx)
			_ = client.Disconnect(ctx)
		}
	})
	return uri
}

func TestReadinessRequiresKeyAndSettings(t *testing.T) {
	uri := testMongoURI(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	keyPath := filepath.Join(t.TempDir(), "master.key")
	keys, err := secrets.CreateKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	boot := Bootstrap{Env: EnvTest, MongoURI: uri, MasterKeyFile: keyPath, LogLevel: "error"}
	c, err := newContainer(logger.Discard(), boot, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	if err := c.checkMongo(ctx); err != nil {
		t.Fatalf("mongo: %v", err)
	}
	var out bytes.Buffer
	if code := ensureSchema(ctx, c, IO{Out: &out, Err: &out}); code != ExitOK {
		t.Fatalf("schema: %s", out.String())
	}
	// Running it again on a database that has the schema is a no-op.
	if code := ensureSchema(ctx, c, IO{Out: &out, Err: &out}); code != ExitOK {
		t.Fatalf("schema again: %s", out.String())
	}
	if c.checkMasterKey(ctx) == nil {
		t.Fatal("master key check passed before init")
	}
	if _, err := c.Installation.Initialize(ctx, installation.Input{WorkspaceName: "Acme", AdminEmail: "a@example.com", AdminName: users.PersonName{Given: "A", Family: "Admin"}, AdminPassword: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	if err := c.checkMasterKey(ctx); err != nil {
		t.Fatalf("master key after init: %v", err)
	}
	if c.checkSettings(ctx) == nil {
		t.Fatal("settings check passed before snapshot load")
	}
	if _, err := c.Settings.Refresh(ctx); err != nil || c.checkSettings(ctx) != nil {
		t.Fatalf("settings: %v", err)
	}

	// The same database with a different master key is not ready.
	other, _ := secrets.NewKeyring(bytes.Repeat([]byte{9}, secrets.KeySize))
	c2, err := newContainer(logger.Discard(), boot, other)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close(context.Background())
	if err := c2.checkMasterKey(ctx); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong key: %v", err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestInitThenServe exercises the real commands: non-interactive init twice,
// then serve until ready, then graceful shutdown.
func TestInitThenServe(t *testing.T) {
	uri := testMongoURI(t)
	keyPath := filepath.Join(t.TempDir(), "secrets", "master.key")
	redisURI := os.Getenv("OPEN_KASE_TEST_REDIS_URI")
	if redisURI == "" {
		redisURI = "redis://127.0.0.1:1/0" // unreachable: Redis is optional for readiness
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("MONGODB_URI", uri)
	t.Setenv("REDIS_URI", redisURI)
	t.Setenv("OPEN_KASE_MASTER_KEY_FILE", keyPath)
	t.Setenv("OPEN_KASE_LOG_LEVEL", "error")

	runInitCmd := func() (int, string) {
		pw := filepath.Join(t.TempDir(), "pw")
		os.WriteFile(pw, []byte("correct horse battery\n"), 0o600)
		in, _ := os.Open(pw)
		defer in.Close()
		var out bytes.Buffer
		code := Execute(context.Background(), []string{"init", "--workspace-name", "Acme Support",
			"--admin-email", "admin@example.com", "--admin-given-name", "Ada", "--admin-family-name", "Admin", "--password-stdin"},
			IO{In: in, Out: &out, Err: &out})
		return code, out.String()
	}
	code, out := runInitCmd()
	if code != ExitOK || !strings.Contains(out, "Created master key") || !strings.Contains(out, "Administrator admin@example.com created") {
		t.Fatalf("first init (%d): %s", code, out)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	code, out = runInitCmd()
	if code != ExitOK || !strings.Contains(out, "already exists") || strings.Contains(out, "Created master key") {
		t.Fatalf("second init (%d): %s", code, out)
	}

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var serveOut bytes.Buffer
	go func() {
		done <- Execute(ctx, []string{"serve", "--http-addr", addr}, IO{In: devNull(t), Out: &serveOut, Err: &serveOut})
	}()

	base := "http://" + addr
	deadline := time.Now().Add(20 * time.Second)
	var ready map[string]any
	for {
		resp, err := http.Get(base + "/health/ready")
		if err == nil {
			json.NewDecoder(resp.Body).Decode(&ready)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server never became ready: %v %v\n%s", err, ready, serveOut.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	resp, err := http.Get(base + "/api/v1/meta")
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("X-Request-ID") == "" {
		t.Fatalf("meta: %v %v", resp, err)
	}
	resp.Body.Close()

	cancel()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("serve exit %d: %s", code, serveOut.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not shut down")
	}
}
