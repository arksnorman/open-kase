package runtime

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/package/logger"
)

// routeRequest sends one request for a documented route with every path
// parameter filled in. Bodies are the documented example (or {}), so a
// handler that validated before authorizing would show up as a 4xx other
// than 401/403/404.
func (h *harness) routeRequest(rt transporthttp.RouteAuth, params map[string]string, opts ...reqOpt) result {
	h.t.Helper()
	segs := strings.Split(rt.Path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, ":") {
			v, ok := params[s[1:]]
			if !ok {
				v = "0123456789abcdef01234567"
			}
			segs[i] = v
		}
	}
	path := strings.Join(segs, "/")
	opts = append(opts, header("Idempotency-Key", "route-auth-"+strings.ReplaceAll(path, "/", "-")+rt.Method), header("If-Match", "1"),
		header("Origin", testOrigin))
	if rt.Multipart {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "a.txt")
		fw.Write([]byte("hello"))
		mw.Close()
		req := httptest.NewRequest(rt.Method, path, &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		for _, o := range opts {
			o(req)
		}
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, req)
		return result{t: h.t, Code: w.Code, Body: w.Body.Bytes(), Header: w.Header()}
	}
	var body any
	switch rt.Method {
	case "POST", "PUT", "PATCH":
		body = rt.Example
		if body == nil {
			body = map[string]any{}
		}
	}
	return h.do(rt.Method, path, body, opts...)
}

// TestEveryRouteRequiresAuthentication enumerates the OpenAPI operation
// table (which TestOpenAPICoversEveryRoute keeps equal to the router) and
// checks that every non-public route refuses anonymous requests, that
// admin routes refuse an agent and an integration, and that staff-only
// routes refuse an integration.
func TestEveryRouteRequiresAuthentication(t *testing.T) {
	f := newTicketFixture(t)
	tk := f.createTicket(f.dana.Access, map[string]any{}).JSON()
	params := map[string]string{"t": refOf(tk), "g": f.triage, "u": f.dana.UserId, "visibility": "public"}
	routes := transporthttp.RouteAuthTable()
	if len(routes) < 100 {
		t.Fatalf("only %d routes documented", len(routes))
	}
	var checkedAnon, checkedAdmin int
	for _, rt := range routes {
		name := rt.Method + " " + rt.Path
		if rt.Public {
			continue
		}
		t.Run(name, func(t *testing.T) {
			// Anonymous: no credential at all, and a malformed one.
			for _, opts := range [][]reqOpt{nil, {header("Authorization", "Bearer not-a-token")}, {header("Authorization", "Basic YTpi")}} {
				r := f.routeRequest(rt, params, opts...)
				want := 401
				if rt.Cookie && len(opts) == 0 {
					// Cookie routes without a cookie: 401 (or 403 for the CSRF check).
					if r.Code != 401 && r.Code != 403 {
						t.Errorf("%s anonymous: status %d: %s", name, r.Code, r.Body)
					}
					continue
				}
				if rt.Cookie {
					continue // cookie routes ignore Authorization
				}
				if r.Code != want {
					t.Errorf("%s anonymous %v: status %d, want %d: %s", name, opts != nil, r.Code, want, r.Body)
				}
			}
			checkedAnon++
			if rt.Cookie {
				return
			}
			if rt.AdminOnly {
				for who, tok := range map[string]string{"agent": f.dana.Access, "integration": f.integration} {
					r := f.routeRequest(rt, params, bearer(tok))
					if r.Code != 403 && r.Code != 404 {
						t.Errorf("%s as %s: status %d, want 403: %s", name, who, r.Code, r.Body)
					}
				}
				checkedAdmin++
			}
			if !rt.Integrations && !rt.AdminOnly && groupScopedRoute(rt.Path) {
				// Staff-only routes refuse even a fully scoped integration.
				r := f.routeRequest(rt, params, bearer(f.integration))
				if r.Code != 403 && r.Code != 404 {
					t.Errorf("%s as same-project integration: status %d, want 403/404: %s", name, r.Code, r.Body)
				}
			}
		})
	}
	t.Logf("anonymous %d, admin-only %d routes", checkedAnon, checkedAdmin)

	// The MCP endpoint is not in the REST table.
	accept := header("Accept", "application/json, text/event-stream")
	for _, m := range []string{"POST", "GET", "DELETE"} {
		r := f.do(m, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, accept)
		if r.Code != 401 {
			t.Errorf("%s /mcp anonymous: %d %s", m, r.Code, r.Body)
		}
	}
	f.do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, accept, bearer("kase_live_bogus")).expect(401)

	// Nothing the sweep did changed #1 or created data in MAIL (the
	// sweep sends bodies to create routes as other principals).
	if got := f.get(f.dana.Access, refOf(tk)).expect(200).JSON(); got["subject"] != tk["subject"] || got["version"] != tk["version"] {
		t.Fatalf("ticket changed: %v", got)
	}
}

// TestRateLimits checks the per-principal and per-IP request limits with
// small overrides: the limit applies per principal and class, answers 429
// with Retry-After, and leaves other principals and classes alone.
func TestRateLimits(t *testing.T) {
	f := newTicketFixture(t)
	r, err := transporthttp.NewRouter(transporthttp.Deps{Logger: logger.Discard(), Readiness: transporthttp.NewReadiness(logger.Discard()),
		Services: f.c.httpServices(), RateLimits: map[string]transporthttp.RateLimit{
			transporthttp.LimitTicketCreate:   {PerMinute: 1, Burst: 2},
			transporthttp.LimitAuth:           {PerMinute: 1, Burst: 3},
			transporthttp.LimitConnectionTest: {PerMinute: -1},
		}})
	if err != nil {
		t.Fatal(err)
	}
	create := func(tok string) result {
		return f.doWith(r, "POST", "/api/v1/tickets", map[string]any{"subject": "Rate", "body": "x",
			"requester": map[string]any{"name": "P", "email": "p@acme.example"}}, bearer(tok))
	}
	create(f.dana.Access).expect(201)
	create(f.dana.Access).expect(201)
	lim := create(f.dana.Access).expect(429, "rateLimited")
	if ra := lim.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After %q", ra)
	}
	// Another principal has its own bucket; other classes are unaffected.
	create(f.marco.Access).expect(201)
	create(f.integration).expect(201)
	f.doWith(r, "GET", "/api/v1/tickets", nil, bearer(f.dana.Access)).expect(200)

	// Auth routes are limited per client IP, before any password check.
	login := func(ip string) result {
		return f.doWith(r, "POST", "/api/v1/auth/login", map[string]string{"login": "nobody@example.com", "password": "wrong password"},
			header("Origin", testOrigin), func(req *http.Request) { req.RemoteAddr = ip + ":1234" })
	}
	for i := 0; i < 3; i++ {
		if c := login("192.0.2.10").Code; c != 401 && c != 429 {
			t.Fatalf("login %d: %d", i, c)
		}
	}
	login("192.0.2.10").expect(429, "rateLimited")
	if p := login("192.0.2.11").Problem(); p.Code == "rateLimited" {
		// A different client has its own bucket (it may still meet the
		// MongoDB per-account throttle, which answers loginThrottled).
		t.Fatal("second client rate limited")
	}
}

// TestSecurityReviewRegressions covers the Milestone 10 review findings.
func TestSecurityReviewRegressions(t *testing.T) {
	f := newTicketFixture(t)
	f.updateSettings(func(s *settings.Settings) { s.Product.BaseURL = "https://support.example.com" })

	{ // account tokens are checked before hashing
		before := auth.ArgonComputations()
		bogus := strings.Repeat("A", 43)
		f.do("POST", "/api/v1/auth/password-reset/complete", map[string]any{"token": bogus, "password": "a long enough password"},
			header("Origin", testOrigin)).expect(422)
		f.do("POST", "/api/v1/auth/invitations/accept", map[string]any{"token": bogus, "password": "a long enough password"},
			header("Origin", testOrigin)).expect(422)
		if n := auth.ArgonComputations() - before; n != 0 {
			t.Fatalf("%d password hashes for invalid tokens", n)
		}
	}

	{ // staged uploads are capped per uploader
		r, err := transporthttp.NewRouter(transporthttp.Deps{Logger: logger.Discard(), Readiness: transporthttp.NewReadiness(logger.Discard()),
			Services: f.c.httpServices(), RateLimits: map[string]transporthttp.RateLimit{transporthttp.LimitUpload: {PerMinute: -1}}})
		if err != nil {
			t.Fatal(err)
		}
		prev := f.h
		f.h = r
		defer func() { f.h = prev }()
		for i := 0; i < tickets.MaxPendingUploads; i++ {
			f.upload(f.dana.Access, fmt.Sprintf("f%d.txt", i), []byte("hello")).expect(201)
		}
		f.upload(f.dana.Access, "one-too-many.txt", []byte("hello")).expect(429, "tooManyPendingUploads")
		f.upload(f.marco.Access, "other-uploader.txt", []byte("hello")).expect(201)
		f.upload(f.integration, "integration.txt", []byte("hello")).expect(201)
	}

	{ // stored SMTP password is not reused for a new destination
		put := func(host string, password any) result {
			cur := f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200).JSON()
			body := map[string]any{"enabled": true, "host": host, "port": 587, "username": "mailer",
				"fromAddress": "support@example.com", "fromName": "Support", "tlsMode": "starttls"}
			if password != nil {
				body["password"] = password
			}
			return f.do("PUT", "/api/v1/settings/smtp", body, bearer(f.admin.Access), ifMatchV(int64(cur["configVersion"].(float64))))
		}
		put("smtp.example.com", "first password").expect(200)
		put("smtp.example.com", nil).expect(200) // same destination keeps it
		if r := put("smtp.attacker.example", nil).expect(422); r.Problem().Errors[0].Field != "smtp.password" {
			t.Fatalf("field: %s", r.Body)
		}
		put("smtp.attacker.example", "").expect(200) // explicitly cleared
		if got := f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200).JSON(); got["passwordConfigured"] != false {
			t.Fatalf("password kept: %v", got)
		}
		put("smtp.example.com", "second password").expect(200)
	}

	{ // integrations do not see staff user IDs
		f.createTicket(f.integration, map[string]any{})
		f.reply(f.dana.Access, "1", "public", "We are looking into it.")
		var staff map[string]any
		for _, m := range f.do("GET", tpath("1", "/messages"), nil, bearer(f.integration)).expect(200).Items() {
			if a := obj(m, "author"); a["context"] == "staff" {
				staff = a
			}
		}
		if staff == nil || staff["id"] != "" || staff["name"] != "Dana Okafor" {
			t.Fatalf("staff author for integration: %v", staff)
		}
		for _, m := range f.do("GET", tpath("1", "/messages"), nil, bearer(f.marco.Access)).expect(200).Items() {
			if a := obj(m, "author"); a["context"] == "staff" && a["id"] != f.dana.UserId {
				t.Fatalf("staff author for staff: %v", a)
			}
		}
	}
}
