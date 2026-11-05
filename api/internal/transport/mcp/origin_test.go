package mcp

import (
	"net/http/httptest"
	"testing"
)

func TestCheckOrigin(t *testing.T) {
	h := &Handler{d: Deps{BaseURL: func() string { return "https://support.example.com" }}}
	cases := []struct {
		host, origin string
		ok           bool
	}{
		{"support.example.com", "", true},
		{"support.example.com", "https://support.example.com", true},
		{"support.example.com", "http://support.example.com", false},
		{"evil.example", "http://evil.example", false}, // DNS rebinding: Host and Origin agree
		{"127.0.0.1:8080", "http://localhost:6274", true},
		{"support.example.com", "http://localhost:6274", false},
		{"support.example.com", "null", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "http://"+c.host+"/mcp", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if err := h.CheckOrigin(r); (err == nil) != c.ok {
			t.Errorf("host %s origin %q: err %v", c.host, c.origin, err)
		}
	}
}
