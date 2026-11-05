package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// A request whose handler panics is answered 500 and still gets its access
// record, which carries the same request ID as the panic record (review
// P-OPS-1: the panic used to unwind past the access log's write).
func TestPanickingRequestIsAccessLogged(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	var buf bytes.Buffer
	r := gin.New()
	r.Use(baseMiddleware(slog.New(slog.NewJSONHandler(&buf, nil)))...)
	r.GET("/boom/:id", func(*gin.Context) { panic("boom") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom/1", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	var access, panicked map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		switch m["msg"] {
		case "http request":
			access = m
		case "panic recovered":
			panicked = m
		}
	}
	if access == nil {
		t.Fatalf("no access record for the panicking request; log:\n%s", buf.String())
	}
	if access["status"] != float64(500) || access["route"] != "/boom/:id" || access["level"] != "ERROR" {
		t.Fatalf("access record %v", access)
	}
	if panicked == nil || panicked["requestId"] != access["requestId"] || w.Header().Get(headerRequestID) != access["requestId"] {
		t.Fatalf("request IDs: panic %v, access %v, header %q", panicked, access["requestId"], w.Header().Get(headerRequestID))
	}
}
