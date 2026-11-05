package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/installation"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/logger"
	"github.com/arksnorman/open-kase/api/package/secrets"
)

// testPasswordParams keep Argon2id fast in tests; production parameters are
// covered by the auth package's own tests.
var testPasswordParams = auth.Argon2Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const (
	adminEmail    = "admin@example.com"
	adminPassword = "correct horse battery"
	testOrigin    = "http://example.com" // httptest requests use Host example.com
)

type harness struct {
	t   *testing.T
	log *slog.Logger // nil: discard
	// rawKey is the master key (to write a key file for CLI commands).
	rawKey []byte
	uri    string
	keys   *secrets.Keyring
	c      *Container
	h      http.Handler
}

// newHarness initializes an empty migrated database (pooled, see
// testdb_pool_test.go) and returns the real
// router wired exactly as `serve` wires it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithLog(t, nil)
}

// newHarnessWithLog is newHarness with the container and router logging to
// log (used to inspect what reaches the logs).
func newHarnessWithLog(t *testing.T, log *slog.Logger) *harness {
	t.Helper()
	uri, fresh := pooledTestDB(t)
	prev := passwordParams
	passwordParams = testPasswordParams
	t.Cleanup(func() { passwordParams = prev })

	key := make([]byte, secrets.KeySize)
	rand.Read(key)
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, uri: uri, keys: keys, log: log, rawKey: key}
	h.c = h.container()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if fresh {
		if err := h.c.Store.EnsureSchema(ctx); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	if _, err := h.c.Installation.Initialize(ctx, installation.Input{WorkspaceName: "Acme", AdminEmail: adminEmail,
		AdminName: users.PersonName{Given: "Ada", Family: "Admin"}, AdminPassword: adminPassword}); err != nil {
		t.Fatal(err)
	}
	h.h = h.router(h.c)
	return h
}

func (h *harness) container() *Container {
	h.t.Helper()
	boot := Bootstrap{Env: EnvTest, MongoURI: h.uri, LogLevel: "error"}
	c, err := newContainer(h.logger(), boot, h.keys)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func (h *harness) router(c *Container) http.Handler {
	h.t.Helper()
	if _, err := c.Settings.Refresh(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	r, err := transporthttp.NewRouter(transporthttp.Deps{Logger: h.logger(),
		Readiness: transporthttp.NewReadiness(h.logger()), Services: c.httpServices(), MCP: c.mcpHandler()})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) logger() *slog.Logger {
	if h.log != nil {
		return h.log
	}
	return logger.Discard()
}

type reqOpt func(*http.Request)

func bearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func cookie(name, value string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }
}

func ifMatchV(v int64) reqOpt { return header("If-Match", jsonString(v)) }

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

type result struct {
	t       *testing.T
	Code    int
	Body    []byte
	Header  http.Header
	Cookies map[string]*http.Cookie
}

// JSON decodes the body into a generic map.
func (r result) JSON() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("decode %q: %v", r.Body, err)
	}
	return m
}

func (r result) Str(key string) string { s, _ := r.JSON()[key].(string); return s }

// Ref is the ticket number of a ticket response as a path segment ("12").
func (r result) Ref() string { return refOf(r.JSON()) }

// refOf is the number of a ticket JSON object as a path segment ("12").
func refOf(m map[string]any) string {
	n, _ := m["number"].(float64)
	return strconv.FormatInt(int64(n), 10)
}

// tnum is a domain ticket's number as a reference ("12").
func tnum(t tickets.Ticket) string { return strconv.FormatInt(t.Number, 10) }

func (r result) Version() int64 { v, _ := r.JSON()["version"].(float64); return int64(v) }

func (r result) Items() []map[string]any {
	raw, _ := r.JSON()["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}

func (r result) Problem() fault.Problem {
	var p fault.Problem
	json.Unmarshal(r.Body, &p)
	return p
}

// expect fails unless the status (and, for problems, the code) match.
func (r result) expect(status int, code ...string) result {
	r.t.Helper()
	if r.Code != status {
		r.t.Fatalf("status %d, want %d: %s", r.Code, status, r.Body)
	}
	if len(code) > 0 && r.Problem().Code != code[0] {
		r.t.Fatalf("problem code %q, want %q: %s", r.Problem().Code, code[0], r.Body)
	}
	return r
}

func (h *harness) do(method, path string, body any, opts ...reqOpt) result {
	h.t.Helper()
	return h.doWith(h.h, method, path, body, opts...)
}

func (h *harness) doWith(handler http.Handler, method, path string, body any, opts ...reqOpt) result {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	res := w.Result()
	cookies := map[string]*http.Cookie{}
	for _, c := range res.Cookies() {
		cookies[c.Name] = c
	}
	return result{t: h.t, Code: w.Code, Body: w.Body.Bytes(), Header: w.Header(), Cookies: cookies}
}

type session struct {
	Access  string
	Refresh string
	CSRF    string
	UserId  string
}

func (h *harness) login(email, password string) session {
	h.t.Helper()
	r := h.do("POST", "/api/v1/auth/login", map[string]string{"login": email, "password": password}, header("Origin", testOrigin)).expect(200)
	user, _ := r.JSON()["user"].(map[string]any)
	return session{Access: r.Str("accessToken"), Refresh: r.Cookies["kase_refresh"].Value, CSRF: r.Cookies["kase_csrf"].Value, UserId: user["id"].(string)}
}

func (h *harness) admin() session { return h.login(adminEmail, adminPassword) }

// refresh presents s's refresh token with valid CSRF and Origin.
func (h *harness) refresh(s session) result {
	h.t.Helper()
	return h.do("POST", "/api/v1/auth/refresh", nil, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", s.CSRF),
		header("X-CSRF-Token", s.CSRF), header("Origin", testOrigin))
}

// createUser creates an active agent user and returns its ID. The name is
// split into given, middle and family name; a one-word name gets the
// family name "Test".
func (h *harness) createUser(admin session, email, name string) string {
	h.t.Helper()
	r := h.do("POST", "/api/v1/users", nameBody(name, map[string]any{"email": email, "password": "agent password 1"}), bearer(admin.Access)).expect(201)
	return r.Str("id")
}

// nameBody adds givenName, middleName and familyName for name to body.
func nameBody(name string, body map[string]any) map[string]any {
	n := users.SplitName(name)
	if n.Family == "" {
		n.Family = "Test"
	}
	body["givenName"], body["familyName"] = n.Given, n.Family
	if n.Middle != "" {
		body["middleName"] = n.Middle
	}
	return body
}

// project returns the deployment's project (created by init with key MAIL).
func (h *harness) project(s session) result {
	h.t.Helper()
	return h.do("GET", "/api/v1/project", nil, bearer(s.Access)).expect(200)
}

// makeAdmin gives a user the admin role.
func (h *harness) makeAdmin(admin session, uid string) {
	h.t.Helper()
	v := h.do("GET", "/api/v1/users/"+uid, nil, bearer(admin.Access)).expect(200).Version()
	h.do("PATCH", "/api/v1/users/"+uid, map[string]any{"isAdmin": true}, bearer(admin.Access), ifMatchV(v)).expect(200)
}
