package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/auth"
	mongostore "github.com/arksnorman/open-kase/api/internal/persistence/mongo"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/clock"
)

// rawDB opens the harness database directly for assertions about storage.
func (h *harness) rawDB() *mongo.Database {
	h.t.Helper()
	client, err := mongo.Connect(options.Client().ApplyURI(h.uri))
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { client.Disconnect(context.Background()) })
	return client.Database(h.c.Store.Database())
}

func (h *harness) auditActions() []string {
	h.t.Helper()
	evs, err := h.c.Audit.RecentAudit(context.Background(), 200)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Action+":"+e.Outcome)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestLoginSuccessAndFailure(t *testing.T) {
	h := newHarness(t)

	r := h.do("POST", "/api/v1/auth/login", map[string]string{"login": "ADMIN@example.com ", "password": adminPassword}, header("Origin", testOrigin)).expect(200)
	m := r.JSON()
	user := m["user"].(map[string]any)
	if m["expiresIn"].(float64) != 600 || user["email"] != adminEmail || user["isAdmin"] != true || user["theme"] != "system" || r.Str("accessToken") == "" {
		t.Fatalf("login body: %s", r.Body)
	}
	if len(m) != 5 || m["mfaRequired"] != false || m["mfaEnrolmentRequired"] != false {
		t.Fatalf("login response has fields beyond the contract: %s", r.Body)
	}
	rc, cc := r.Cookies["kase_refresh"], r.Cookies["kase_csrf"]
	if rc == nil || !rc.HttpOnly || rc.Path != "/api/v1/auth" || rc.SameSite != http.SameSiteStrictMode || !rc.Secure || rc.MaxAge <= 0 {
		t.Fatalf("refresh cookie: %+v", rc)
	}
	if cc == nil || cc.HttpOnly || cc.Path != "/" || cc.SameSite != http.SameSiteStrictMode || !cc.Secure {
		t.Fatalf("csrf cookie: %+v", cc)
	}
	// Access token is an EdDSA JWT with a key ID.
	if parts := strings.Split(r.Str("accessToken"), "."); len(parts) != 3 {
		t.Fatal("access token is not a JWT")
	}
	h.do("GET", "/api/v1/me", nil, bearer(r.Str("accessToken"))).expect(200)

	// Wrong password, unknown account and malformed input do not differ.
	wrong := h.do("POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": "nope"}).expect(401, "invalidCredentials")
	unknown := h.do("POST", "/api/v1/auth/login", map[string]string{"login": "ghost@example.com", "password": "nope"}).expect(401, "invalidCredentials")
	if wrong.Problem().Detail != unknown.Problem().Detail || len(wrong.Cookies) != 0 {
		t.Fatal("login failures are distinguishable or set cookies")
	}
	h.do("POST", "/api/v1/auth/login", map[string]any{"login": adminEmail, "password": "x", "remember": true}).expect(422, "validationFailed")
	h.do("POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": adminPassword}, header("Origin", "https://evil.example")).expect(403, "originRejected")

	// Requests without or with a garbage token are rejected.
	h.do("GET", "/api/v1/me", nil).expect(401, "authenticationRequired")
	h.do("GET", "/api/v1/me", nil, bearer("not.a.jwt")).expect(401, "invalidToken")
	acts := h.auditActions()
	if !contains(acts, "auth.login:success") || !contains(acts, "auth.login:failure") {
		t.Fatalf("login audit missing: %v", acts)
	}
}

func TestLoginThrottle(t *testing.T) {
	h := newHarness(t)
	h.createUser(h.admin(), "other@example.com", "Other")
	for i := 0; i < 5; i++ {
		h.do("POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": "wrong"}).expect(401, "invalidCredentials")
	}
	// The sixth attempt from this client is throttled even with the right
	// password (other clients: TestLoginLockoutIsPerClient).
	r := h.do("POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": adminPassword}).expect(429, "loginThrottled")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After header")
	}
	// Throttling is per account: another user can still sign in.
	h.login("other@example.com", "agent password 1")
	// The counter lives in MongoDB, so a second API process enforces it too.
	other := h.router(h.container())
	h.doWith(other, "POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": adminPassword}).expect(429, "loginThrottled")
}

func TestRefreshRotationAndReuseRevokesFamily(t *testing.T) {
	h := newHarness(t)
	s := h.admin()

	r1 := h.refresh(s).expect(200)
	next := r1.Cookies["kase_refresh"]
	if next == nil || next.Value == s.Refresh || r1.Str("accessToken") == "" {
		t.Fatalf("refresh did not rotate: %s", r1.Body)
	}
	s2 := s
	s2.Refresh, s2.Access = next.Value, r1.Str("accessToken")
	h.do("GET", "/api/v1/me", nil, bearer(s2.Access)).expect(200)

	// Presenting an already-rotated token after its successor was used is
	// reuse: the whole family is revoked, including the newest refresh token
	// and access tokens.
	r2 := h.refresh(s2).expect(200)
	s3 := s2
	s3.Refresh, s3.Access = r2.Cookies["kase_refresh"].Value, r2.Str("accessToken")
	h.refresh(s).expect(401, "refreshTokenReused")
	h.refresh(s3).expect(401, "invalidRefreshToken")
	h.do("GET", "/api/v1/me", nil, bearer(s3.Access)).expect(401, "sessionRevoked")
	h.do("GET", "/api/v1/me", nil, bearer(s2.Access)).expect(401, "sessionRevoked")
	h.do("GET", "/api/v1/me", nil, bearer(s.Access)).expect(401, "sessionRevoked")
	if !contains(h.auditActions(), "auth.refreshReuseDetected:success") {
		t.Fatal("reuse not audited")
	}

	// Refresh tokens are stored only as SHA-256 hashes.
	db := h.rawDB()
	sum := sha256.Sum256([]byte(s.Refresh))
	if n, _ := db.Collection("refreshTokens").CountDocuments(context.Background(), bson.D{{Key: "tokenHash", Value: hex.EncodeToString(sum[:])}}); n != 1 {
		t.Fatalf("refresh hash not found (%d)", n)
	}
	if n, _ := db.Collection("refreshTokens").CountDocuments(context.Background(), bson.D{{Key: "tokenHash", Value: s.Refresh}}); n != 0 {
		t.Fatal("refresh token stored in clear")
	}

	// A fresh login is unaffected.
	h.refresh(h.admin()).expect(200)
}

func TestCSRFAndOriginOnCookieRoutes(t *testing.T) {
	h := newHarness(t)
	s := h.admin()
	post := func(path string, opts ...reqOpt) result {
		return h.do("POST", path, nil, opts...)
	}
	for _, path := range []string{"/api/v1/auth/refresh", "/api/v1/auth/logout"} {
		// Missing CSRF header.
		post(path, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", s.CSRF), header("Origin", testOrigin)).expect(403, "csrfFailed")
		// Header does not match the cookie.
		post(path, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", s.CSRF), header("X-CSRF-Token", "forged"), header("Origin", testOrigin)).expect(403, "csrfFailed")
		// Matching header and cookie that do not belong to this session
		// (e.g. a cookie planted by a sibling subdomain).
		post(path, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", "planted"), header("X-CSRF-Token", "planted"), header("Origin", testOrigin)).expect(403, "csrfFailed")
		// Missing and cross-site Origin.
		post(path, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", s.CSRF), header("X-CSRF-Token", s.CSRF)).expect(403, "originRejected")
		post(path, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", s.CSRF), header("X-CSRF-Token", s.CSRF), header("Origin", "https://evil.example")).expect(403, "originRejected")
	}
	// None of the rejected attempts consumed or revoked the session.
	r := h.refresh(s).expect(200)
	s.Refresh, s.Access = r.Cookies["kase_refresh"].Value, r.Str("accessToken")

	// Logout revokes the session and clears both cookies.
	out := h.do("POST", "/api/v1/auth/logout", nil, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", s.CSRF),
		header("X-CSRF-Token", s.CSRF), header("Origin", testOrigin)).expect(204)
	if c := out.Cookies["kase_refresh"]; c == nil || c.MaxAge >= 0 {
		t.Fatalf("refresh cookie not cleared: %+v", c)
	}
	if c := out.Cookies["kase_csrf"]; c == nil || c.MaxAge >= 0 {
		t.Fatalf("csrf cookie not cleared: %+v", c)
	}
	h.do("GET", "/api/v1/me", nil, bearer(s.Access)).expect(401, "sessionRevoked")
	h.refresh(s).expect(401)
	if !contains(h.auditActions(), "auth.logout:success") {
		t.Fatal("logout not audited")
	}
}

func TestDisabledUserTokenRejected(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := h.createUser(admin, "agent@example.com", "Agent")
	agent := h.login("agent@example.com", "agent password 1")
	h.do("GET", "/api/v1/me", nil, bearer(agent.Access)).expect(200)

	u := h.do("GET", "/api/v1/users/"+uid, nil, bearer(admin.Access)).expect(200)
	h.do("PATCH", "/api/v1/users/"+uid, map[string]any{"status": "disabled"}, bearer(admin.Access)).expect(428, "preconditionRequired")
	h.do("PATCH", "/api/v1/users/"+uid, map[string]any{"status": "disabled"}, bearer(admin.Access), ifMatchV(u.Version()+1)).expect(409, "versionConflict")
	d := h.do("PATCH", "/api/v1/users/"+uid, map[string]any{"status": "disabled"}, bearer(admin.Access), ifMatchV(u.Version())).expect(200)
	if d.Str("status") != "disabled" || d.Version() != u.Version()+1 {
		t.Fatalf("disable: %s", d.Body)
	}
	// The still-unexpired access token is rejected on the next request,
	// refresh fails and sign-in fails without revealing the account state.
	if r := h.do("GET", "/api/v1/me", nil, bearer(agent.Access)); r.Code != 401 {
		t.Fatalf("disabled user's token accepted: %d %s", r.Code, r.Body)
	}
	h.refresh(agent).expect(401)
	h.do("POST", "/api/v1/auth/login", map[string]string{"login": "agent@example.com", "password": "agent password 1"}).expect(401, "invalidCredentials")
	if !contains(h.auditActions(), "user.disabled:success") || !contains(h.auditActions(), "user.created:success") {
		t.Fatal("identity changes not audited")
	}

	// Administrators cannot disable themselves or remove the last admin.
	me := h.do("GET", "/api/v1/users/"+admin.UserId, nil, bearer(admin.Access)).expect(200)
	h.do("PATCH", "/api/v1/users/"+admin.UserId, map[string]any{"status": "disabled"}, bearer(admin.Access), ifMatchV(me.Version())).expect(409, "cannotDisableSelf")

	// Non-administrators cannot manage users.
	h.createUser(admin, "plain@example.com", "Plain")
	plain := h.login("plain@example.com", "agent password 1")
	h.do("GET", "/api/v1/users", nil, bearer(plain.Access)).expect(403, "forbidden")
	h.do("POST", "/api/v1/users", map[string]any{"email": "x@example.com", "givenName": "X", "familyName": "Y"}, bearer(plain.Access)).expect(403)
}

func TestMePreferencesAndPasswordChange(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.createUser(admin, "agent@example.com", "Agent")
	a1 := h.login("agent@example.com", "agent password 1")
	a2 := h.login("agent@example.com", "agent password 1")

	r := h.do("PUT", "/api/v1/me/preferences", map[string]string{"theme": "dark"}, bearer(a1.Access)).expect(200)
	if r.Str("theme") != "dark" || r.Str("email") != "agent@example.com" {
		t.Fatalf("preferences: %s", r.Body)
	}
	h.do("PUT", "/api/v1/me/preferences", map[string]string{"theme": "neon"}, bearer(a1.Access)).expect(422)
	if h.do("GET", "/api/v1/me", nil, bearer(a1.Access)).Str("theme") != "dark" {
		t.Fatal("theme not persisted")
	}

	h.do("POST", "/api/v1/me/password", map[string]string{"currentPassword": "wrong", "newPassword": "a brand new password"}, bearer(a1.Access)).expect(422)
	h.do("POST", "/api/v1/me/password", map[string]string{"currentPassword": "agent password 1", "newPassword": "short"}, bearer(a1.Access)).expect(422)
	h.do("POST", "/api/v1/me/password", map[string]string{"currentPassword": "agent password 1", "newPassword": "a brand new password"}, bearer(a1.Access)).expect(204)
	// The current session survives; the other session is revoked.
	h.do("GET", "/api/v1/me", nil, bearer(a1.Access)).expect(200)
	h.do("GET", "/api/v1/me", nil, bearer(a2.Access)).expect(401, "sessionRevoked")
	h.do("POST", "/api/v1/auth/login", map[string]string{"login": "agent@example.com", "password": "agent password 1"}).expect(401)
	h.login("agent@example.com", "a brand new password")
	if !contains(h.auditActions(), "user.passwordChanged:success") {
		t.Fatal("password change not audited")
	}
}

func TestSigningKeyPersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	s := h.admin()
	// A second process (same master key and database) verifies the token:
	// the signing key is loaded, not regenerated.
	other := h.router(h.container())
	h.doWith(other, "GET", "/api/v1/me", nil, bearer(s.Access)).expect(200)
	h.doWith(other, "POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": adminPassword}).expect(200)

	db := h.rawDB()
	var keys []bson.M
	cur, err := db.Collection("signingKeys").Find(context.Background(), bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	cur.All(context.Background(), &keys)
	if len(keys) != 1 {
		t.Fatalf("expected exactly one signing key, got %d", len(keys))
	}
	if ct, _ := keys[0]["privateCiphertext"].(string); !strings.HasPrefix(ct, "v1:"+h.keys.ActiveKeyID()+":") {
		t.Fatalf("private key not sealed with the master key: %q", ct)
	}
	if keys[0]["algorithm"] != "EdDSA" {
		t.Fatalf("algorithm %v", keys[0]["algorithm"])
	}
}

// TestConcurrentRefreshFromTwoTabs presents one refresh token twice at the
// same time, as two browser tabs sharing the cookie do when their access
// tokens expire together (or when both are opened at once). The transactions
// conflict on the token document, so exactly one presentation rotates the
// token. The other one is inside the grace window of a token whose successor
// nobody has used yet: it gets an access token for the same session and no
// refresh cookie, so the browser keeps the successor the winner received.
// Neither tab is signed out (OC-AUTH-01).
func TestConcurrentRefreshFromTwoTabs(t *testing.T) {
	h := newHarness(t)
	for round := 0; round < 5; round++ {
		s := h.admin()
		res := make(chan result, 2)
		for i := 0; i < 2; i++ {
			go func() { res <- h.refresh(s) }()
		}
		rotated := 0
		for _, r := range []result{<-res, <-res} {
			r.expect(200)
			if c := r.Cookies["kase_refresh"]; c != nil {
				if c.Value == "" || c.MaxAge <= 0 {
					t.Fatalf("round %d: a concurrent refresh cleared the refresh cookie: %+v", round, c)
				}
				rotated++
				s.Refresh = c.Value
			}
			if r.Cookies["kase_csrf"] != nil {
				t.Fatalf("round %d: refresh changed the CSRF cookie", round)
			}
			h.do("GET", "/api/v1/me", nil, bearer(r.Str("accessToken"))).expect(200)
		}
		if rotated != 1 {
			t.Fatalf("round %d: %d of 2 concurrent refreshes rotated the token, want exactly 1", round, rotated)
		}
		// The session continues with the token the winner received.
		h.refresh(s).expect(200)
	}
	if contains(h.auditActions(), "auth.refreshReuseDetected:success") {
		t.Fatal("a concurrent refresh was recorded as token reuse")
	}
}

// TestRefreshGraceWindowKeepsReplayDetection checks the limits of the grace
// window: the immediately previous token is honoured only for a short time
// and only while its successor is unused, and it never yields a second
// refresh token. A token presented after the window, or after its successor
// was used, is a replay and revokes the family.
func TestRefreshGraceWindowKeepsReplayDetection(t *testing.T) {
	h := newHarness(t)
	clk := clock.NewManual(time.Now().UTC().Truncate(time.Second))
	h.c.UseAuthClock(clk)
	h.h = h.router(h.c)
	rotate := func(s session) session {
		t.Helper()
		r := h.refresh(s).expect(200)
		c := r.Cookies["kase_refresh"]
		if c == nil || c.Value == "" || c.Value == s.Refresh {
			t.Fatalf("refresh did not rotate: %+v", c)
		}
		s.Refresh, s.Access = c.Value, r.Str("accessToken")
		return s
	}

	// Inside the window: an access token, but no new refresh token and no
	// cookie change; as often as the tabs ask.
	s0 := h.admin()
	s1 := rotate(s0)
	clk.Advance(auth.RefreshGrace - time.Second)
	for i := 0; i < 3; i++ {
		g := h.refresh(s0).expect(200)
		if len(g.Cookies) != 0 {
			t.Fatalf("grace refresh set cookies: %v", g.Cookies)
		}
		h.do("GET", "/api/v1/me", nil, bearer(g.Str("accessToken"))).expect(200)
	}
	// After the window the same token is a replay: the family is revoked.
	clk.Advance(2 * time.Second)
	out := h.refresh(s0).expect(401, "refreshTokenReused")
	if c := out.Cookies["kase_refresh"]; c == nil || c.MaxAge >= 0 {
		t.Fatalf("refresh cookie not cleared after reuse: %+v", c)
	}
	h.refresh(s1).expect(401, "invalidRefreshToken")
	h.do("GET", "/api/v1/me", nil, bearer(s1.Access)).expect(401, "sessionRevoked")

	// Inside the window but after the successor was used: also a replay.
	a0 := h.admin()
	a1 := rotate(a0)
	a2 := rotate(a1)
	h.refresh(a0).expect(401, "refreshTokenReused")
	h.refresh(a2).expect(401, "invalidRefreshToken")
	h.do("GET", "/api/v1/me", nil, bearer(a2.Access)).expect(401, "sessionRevoked")

	// The grace applies to the previous token only, not to older ones: with
	// T0 → T1 → T2, T1 is inside its window and T0 is not honoured.
	b0 := h.admin()
	b1 := rotate(b0)
	b2 := rotate(b1)
	h.refresh(b1).expect(200)
	h.refresh(b0).expect(401, "refreshTokenReused")
	h.refresh(b2).expect(401, "invalidRefreshToken")

	// A grace refresh still needs the session's CSRF token.
	c0 := h.admin()
	rotate(c0)
	h.do("POST", "/api/v1/auth/refresh", nil, cookie("kase_refresh", c0.Refresh), cookie("kase_csrf", "planted"),
		header("X-CSRF-Token", "planted"), header("Origin", testOrigin)).expect(403, "csrfFailed")
	n := 0
	for _, a := range h.auditActions() {
		if a == "auth.refreshReuseDetected:success" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d reuse events audited, want 3", n)
	}
}

// TestSelfServiceWriteCannotUndoAdminChange replays a stale user record
// through the theme and password paths after an administrator disabled the
// account: the scoped writes must not restore status or roles.
func TestSelfServiceWriteCannotUndoAdminChange(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := h.createUser(admin, "agent@example.com", "Agent")
	repo := mongostore.NewUsersRepository(h.c.Store)
	ctx := context.Background()
	stale, err := repo.Get(ctx, h.c.Settings.Current().WorkspaceID(), uid)
	if err != nil {
		t.Fatal(err)
	}
	h.do("PATCH", "/api/v1/users/"+uid, map[string]any{"status": "disabled"}, bearer(admin.Access), ifMatchV(stale.Version)).expect(200)

	stale.Theme = "dark"
	if _, err := repo.Apply(ctx, users.Change{User: stale, Only: users.OnlyTheme}); err != nil {
		t.Fatal(err)
	}
	stale.PasswordHash = stale.PasswordHash + "x"
	if _, err := repo.Apply(ctx, users.Change{User: stale, Only: users.OnlyPassword}); err != nil {
		t.Fatal(err)
	}
	got := h.do("GET", "/api/v1/users/"+uid, nil, bearer(admin.Access)).expect(200)
	if got.Str("status") != "disabled" {
		t.Fatalf("stale self-service write re-enabled the user: %s", got.Body)
	}
	h.do("POST", "/api/v1/auth/login", map[string]string{"login": "agent@example.com", "password": "agent password 1"}).expect(401, "invalidCredentials")
}

func TestLoginRejectsCrossSiteForms(t *testing.T) {
	h := newHarness(t)
	body := `{"login":"` + adminEmail + `","password":"` + adminPassword + `"}`
	h.do("POST", "/api/v1/auth/login", body, header("Content-Type", "text/plain")).expect(415, "unsupportedMediaType")
	h.do("POST", "/api/v1/auth/login", body, header("Sec-Fetch-Site", "cross-site")).expect(403, "originRejected")
	h.do("POST", "/api/v1/auth/login", body, header("Content-Type", "application/json; charset=utf-8")).expect(200)
}
