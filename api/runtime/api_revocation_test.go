package runtime

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestRevokedPrincipalsOnMCPAndStream: a revoked session, a disabled user
// and a revoked integration token stop working on the next request on every
// surface: REST, the MCP endpoint and an already open live-update stream.
func TestRevokedPrincipalsOnMCPAndStream(t *testing.T) {
	f := newTicketFixture(t)
	f.c.LiveHeartbeat = 300 * time.Millisecond
	f.h = f.router(f.c)
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	accept := header("Accept", "application/json, text/event-stream")
	mcpList := func(tok string) result {
		return f.do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, accept, bearer(tok))
	}
	ready := func(ch <-chan sseEvent) {
		t.Helper()
		if evs := collect(ch, 5*time.Second, func(e []sseEvent) bool { return len(e) > 0 }); len(evs) == 0 || evs[0].Event != "ready" {
			t.Fatalf("no ready event: %v", evs)
		}
	}
	ended := func(ch <-chan sseEvent, code string) {
		t.Helper()
		end := collect(ch, 5*time.Second, func(e []sseEvent) bool { return len(e) > 0 && e[len(e)-1].Event == "error" })
		if len(end) == 0 || end[len(end)-1].Event != "error" || end[len(end)-1].Data["code"] != code {
			t.Fatalf("stream did not end with %s: %v", code, end)
		}
	}
	mcpList(f.dana.Access).expect(200)
	mcpList(f.marco.Access).expect(200)
	dana, stopDana := openStream(t, ts.URL, f.dana.Access)
	defer stopDana()
	marco, stopMarco := openStream(t, ts.URL, f.marco.Access)
	defer stopMarco()
	ready(dana)
	ready(marco)

	// Dana signs out elsewhere: her unexpired access token is dead everywhere.
	f.do("POST", "/api/v1/auth/logout", nil, cookie("kase_refresh", f.dana.Refresh), cookie("kase_csrf", f.dana.CSRF),
		header("X-CSRF-Token", f.dana.CSRF), header("Origin", testOrigin)).expect(204)
	f.do("GET", "/api/v1/me", nil, bearer(f.dana.Access)).expect(401, "sessionRevoked")
	mcpList(f.dana.Access).expect(401, "sessionRevoked")
	ended(dana, "sessionRevoked")

	// An administrator disables Marco: same on every surface (disabling
	// revokes the sessions in the same transaction, so the session check
	// answers first).
	u := f.do("GET", "/api/v1/users/"+f.marco.UserId, nil, bearer(f.admin.Access)).expect(200)
	f.do("PATCH", "/api/v1/users/"+f.marco.UserId, map[string]any{"status": "disabled"}, bearer(f.admin.Access), ifMatchV(u.Version())).expect(200)
	f.do("GET", "/api/v1/me", nil, bearer(f.marco.Access)).expect(401, "sessionRevoked")
	mcpList(f.marco.Access).expect(401, "sessionRevoked")
	ended(marco, "sessionRevoked")

	// A revoked integration token is refused on both endpoints too.
	created := f.do("POST", "/api/v1/integrations", map[string]any{"name": "Agent tool",
		"scopes": []string{"tickets:read"}}, bearer(f.admin.Access)).expect(201)
	mcpList(created.Str("token")).expect(200)
	f.do("DELETE", "/api/v1/integrations/"+created.Str("id"), nil, bearer(f.admin.Access)).expect(204)
	mcpList(created.Str("token")).expect(401, "invalidToken")
	f.do("GET", "/api/v1/tickets", nil, bearer(created.Str("token"))).expect(401, "invalidToken")
	// Any valid token is admitted to both endpoints (no separate MCP scope).
	f.do("GET", "/api/v1/tickets", nil, bearer(f.readOnlyIntegraton)).expect(200)
	mcpList(f.readOnlyIntegraton).expect(200)
}
