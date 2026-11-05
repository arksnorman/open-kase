package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/logger"
)

func newTestRouter(t *testing.T, checks ...Check) *gin.Engine {
	t.Helper()
	r, err := NewRouter(Deps{Logger: logger.Discard(), Readiness: NewReadiness(logger.Discard(), checks...)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func do(r http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLiveAndRequestID(t *testing.T) {
	r := newTestRouter(t)
	w := do(r, http.MethodGet, "/health/live", nil)
	if w.Code != http.StatusOK || len(w.Header().Get(headerRequestID)) != 24 {
		t.Fatalf("live: %d %q", w.Code, w.Header().Get(headerRequestID))
	}
	w = do(r, http.MethodGet, "/health/live", map[string]string{headerRequestID: "proxy-req-12345"})
	if w.Header().Get(headerRequestID) != "proxy-req-12345" {
		t.Fatal("valid inbound request ID not propagated")
	}
	w = do(r, http.MethodGet, "/health/live", map[string]string{headerRequestID: "bad id\nwith newline"})
	if w.Header().Get(headerRequestID) == "bad id\nwith newline" {
		t.Fatal("invalid inbound request ID accepted")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
}

func TestReadiness(t *testing.T) {
	ok := Check{Name: "mongo", Required: true, Run: func(context.Context) error { return nil }}
	bad := Check{Name: "schema", Required: true, Run: func(context.Context) error { return errors.New("pending migrations") }}
	opt := Check{Name: "redis", Run: func(context.Context) error { return errors.New("down") }}

	w := do(newTestRouter(t, ok, opt), http.MethodGet, "/health/ready", nil)
	var rep ReadyReport
	json.Unmarshal(w.Body.Bytes(), &rep)
	if w.Code != http.StatusOK || rep.Status != "ready" || rep.Checks["redis"] != "degraded" {
		t.Fatalf("ready: %d %s", w.Code, w.Body)
	}

	w = do(newTestRouter(t, ok, bad), http.MethodGet, "/health/ready", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready: %d %s", w.Code, w.Body)
	}
	if json.Unmarshal(w.Body.Bytes(), &rep); rep.Checks["schema"] != "fail" || rep.Status != "notReady" {
		t.Fatalf("report: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "pending migrations") {
		t.Fatal("readiness leaked error detail")
	}
}

func TestProblemResponses(t *testing.T) {
	r := newTestRouter(t)
	r.GET("/api/v1/boom", func(c *gin.Context) { panic("kaboom") })
	r.GET("/api/v1/invalid", func(c *gin.Context) {
		writeError(c, fault.Validation(fault.FieldError{Field: "priority", Code: "outOfRange", Message: "Choose Priority 1 to Priority 4."}))
	})

	w := do(r, http.MethodGet, "/api/v1/nope", nil)
	var p fault.Problem
	json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != 404 || p.Code != "routeNotFound" || p.RequestId == "" || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("404: %d %s", w.Code, w.Body)
	}

	w = do(r, http.MethodGet, "/api/v1/boom", nil)
	json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != 500 || p.Code != "internalError" || strings.Contains(w.Body.String(), "kaboom") {
		t.Fatalf("panic: %d %s", w.Code, w.Body)
	}

	w = do(r, http.MethodGet, "/api/v1/invalid", nil)
	json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != 422 || len(p.Errors) != 1 || p.Errors[0].Field != "priority" {
		t.Fatalf("422: %d %s", w.Code, w.Body)
	}

	w = do(r, http.MethodPost, "/health/live", nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method not allowed: %d", w.Code)
	}

	w = do(r, http.MethodGet, "/api/v1/meta", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"product":"open-kase"`) {
		t.Fatalf("meta: %d %s", w.Code, w.Body)
	}
}
