package http

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/package/logger"
)

func TestSPAFallbackNeverSwallowsAPI(t *testing.T) {
	dir := t.TempDir()
	inline := "console.log('theme')"
	index := "<!doctype html><html><head><script>" + inline + "</script></head><body><div id=app></div><script type=module src=/assets/app.js></script></body></html>"
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644)
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("export {}"), 0o644)
	os.WriteFile(filepath.Join(dir, "favicon.svg"), []byte("<svg/>"), 0o644)
	os.WriteFile(filepath.Join(t.TempDir(), "secret.txt"), []byte("nope"), 0o644)

	r, err := NewRouter(Deps{Logger: logger.Discard(), Readiness: NewReadiness(logger.Discard()), UIDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	isProblem404 := func(path string) {
		t.Helper()
		w := do(r, http.MethodGet, path, nil)
		if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: %d %s %q", path, w.Code, w.Header().Get("Content-Type"), w.Body.String()[:min(40, w.Body.Len())])
		}
	}
	for _, p := range []string{"/api", "/api/v1/nope", "/api/v2/x", "/mcp", "/mcp/session", "/health", "/health/x", "/missing.js", "/assets/nope.css"} {
		isProblem404(p)
	}

	// Client-side routes get index.html with a CSP that allows the inline
	// theme script by hash only.
	for _, p := range []string{"/", "/tickets/MAIL-1", "/login", "/apix", "/healthy"} {
		w := do(r, http.MethodGet, p, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<div id=app>") {
			t.Fatalf("%s: %d", p, w.Code)
		}
		sum := sha256.Sum256([]byte(inline))
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || strings.Contains(csp, "unsafe-eval") ||
			!strings.Contains(csp, "script-src 'self'") || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: headers %v", p, w.Header())
		}
	}
	w := do(r, http.MethodGet, "/assets/app.js", nil)
	if w.Code != 200 || w.Body.String() != "export {}" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", w.Code, w.Header())
	}
	if w := do(r, http.MethodGet, "/favicon.svg", nil); w.Code != 200 {
		t.Fatalf("favicon: %d", w.Code)
	}
	// Traversal cannot escape the UI directory.
	for _, p := range []string{"/../secret.txt", "/assets/../../secret.txt", "/%2e%2e/secret.txt"} {
		if w := do(r, http.MethodGet, p, nil); strings.Contains(w.Body.String(), "nope") {
			t.Fatalf("%s escaped the UI directory", p)
		}
	}
	// Only GET/HEAD fall back to the SPA; API routes keep problem responses.
	if w := do(r, http.MethodPost, "/somewhere", nil); w.Code != 404 {
		t.Fatalf("POST fallback: %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/meta", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "apiVersion") {
		t.Fatalf("meta: %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/health/live", nil); w.Code != 200 {
		t.Fatalf("live: %d", w.Code)
	}
	if _, err := NewRouter(Deps{Logger: logger.Discard(), Readiness: NewReadiness(logger.Discard()), UIDir: t.TempDir()}); err == nil {
		t.Fatal("a UI directory without index.html was accepted")
	}
}

func TestSPAIndexReloadsAfterUIDeploy(t *testing.T) {
	dir := t.TempDir()
	write := func(body string, mod time.Time) {
		p := filepath.Join(dir, "index.html")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mod, mod)
	}
	base := time.Now().Add(-time.Hour)
	write("<!doctype html><script>one()</script><div id=app>v1</div>", base)
	r, err := NewRouter(Deps{Logger: logger.Discard(), Readiness: NewReadiness(logger.Discard()), UIDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w := do(r, http.MethodGet, "/tickets", nil)
	if !strings.Contains(w.Body.String(), "v1") || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("v1: %s %v", w.Body.String(), w.Header())
	}
	csp1 := w.Header().Get("Content-Security-Policy")
	// A UI-only deploy replaces index.html: served without a restart, with
	// the CSP hash of the new inline script.
	write("<!doctype html><script>two()</script><div id=app>v2</div>", base.Add(time.Minute))
	w = do(r, http.MethodGet, "/", nil)
	sum := sha256.Sum256([]byte("two()"))
	if !strings.Contains(w.Body.String(), "v2") || w.Header().Get("Content-Security-Policy") == csp1 ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), base64.StdEncoding.EncodeToString(sum[:])) {
		t.Fatalf("v2 not served: %s %v", w.Body.String(), w.Header())
	}
	// A missing index.html mid-deploy keeps serving the last good copy.
	os.Remove(filepath.Join(dir, "index.html"))
	if w := do(r, http.MethodGet, "/", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "v2") {
		t.Fatalf("during deploy: %d", w.Code)
	}
}

func TestThrottleIPIgnoresUntrustedProxy(t *testing.T) {
	r, _ := NewRouter(Deps{Logger: logger.Discard(), Readiness: NewReadiness(logger.Discard())})
	var got []string
	r.GET("/ip", func(c *gin.Context) { got = append(got, throttleIP(c)) })
	do(r, http.MethodGet, "/ip", nil)
	// A client on a public address cannot opt out by sending its own
	// X-Forwarded-For: it is keyed on the connecting address.
	do(r, http.MethodGet, "/ip", map[string]string{"X-Forwarded-For": "203.0.113.9"})
	from := func(remote string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ip", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.ServeHTTP(w, req)
	}
	// An undeclared reverse proxy on loopback or a private network: no IP key.
	from("127.0.0.1:5000")
	from("10.1.2.3:5000")
	from("[::1]:5000")
	if got[0] != "192.0.2.1" || got[1] != "192.0.2.1" || got[2] != "" || got[3] != "" || got[4] != "" {
		t.Fatalf("throttle IPs: %q", got)
	}
}

// TestSPASecurityHeadersForRealIndex serves the repository's ui/index.html
// (read only) and checks the CSP allows exactly its inline theme script by
// hash, plus the other browser security headers.
func TestSPASecurityHeadersForRealIndex(t *testing.T) {
	src, err := os.ReadFile("../../../../ui/index.html")
	if err != nil {
		t.Skip("ui/index.html not present:", err)
	}
	m := inlineScript.FindSubmatch(src)
	if m == nil || len(strings.TrimSpace(string(m[1]))) == 0 {
		t.Fatal("ui/index.html has no inline theme script; update this test")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), src, 0o644)
	r, err := NewRouter(Deps{Logger: logger.Discard(), Readiness: NewReadiness(logger.Discard()), UIDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	w := do(r, http.MethodGet, "/tickets", nil)
	sum := sha256.Sum256(m[1])
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'",
		"frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if scriptSrc, _, _ := strings.Cut(strings.SplitN(csp, "script-src", 2)[1], ";"); strings.Contains(scriptSrc, "unsafe") {
		t.Errorf("script-src allows unsafe-inline: %s", csp)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
		"Cross-Origin-Opener-Policy": "same-origin"} {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	t.Logf("CSP for ui/index.html: %s", csp)
}
