package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arksnorman/open-kase/api/package/fault"
)

// mcpFixture is the ticket fixture plus a live HTTP server for SDK clients.
type mcpFixture struct {
	*ticketFixture
	srv *httptest.Server
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	f := &mcpFixture{ticketFixture: newTicketFixture(t)}
	f.srv = httptest.NewServer(f.h)
	t.Cleanup(f.srv.Close)
	return f
}

// integration creates a MAIL integration with scopes and returns its ID and token.
func (f *mcpFixture) integration(name string, scopes ...string) (string, string) {
	f.t.Helper()
	r := f.do("POST", "/api/v1/integrations", map[string]any{"name": name, "scopes": scopes}, bearer(f.admin.Access)).expect(201)
	return r.Str("id"), r.Str("token")
}

// bearerTransport adds the Authorization header (and optionally rotates
// between several base URLs, to show there is no session affinity).
type bearerTransport struct {
	token string
	hosts []string
	n     atomic.Int64
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	if len(b.hosts) > 0 {
		host := b.hosts[int(b.n.Add(1))%len(b.hosts)]
		r.URL.Host, r.Host = host, host
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connectMCP(t *testing.T, endpoint, token string, hosts ...string) (*sdk.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "open-kase-test", Version: "1"}, nil)
	tr := &sdk.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: &bearerTransport{token: token, hosts: hosts}}}
	s, err := client.Connect(ctx, tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

func (f *mcpFixture) connect(token string) *sdk.ClientSession {
	f.t.Helper()
	s, err := connectMCP(f.t, f.srv.URL+"/mcp", token)
	if err != nil {
		f.t.Fatalf("connect: %v", err)
	}
	return s
}

// toolResult is a decoded tool call: structured output or a problem.
type toolResult struct {
	Out     map[string]any
	Problem *fault.Problem
	Text    string // raw error text (SDK schema errors are not problems)
	IsError bool
}

func callTool(t *testing.T, s *sdk.ClientSession, name string, args map[string]any) (toolResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if args == nil {
		args = map[string]any{}
	}
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return toolResult{}, err
	}
	var out toolResult
	if res.IsError {
		out.IsError = true
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*sdk.TextContent); ok {
				out.Text = tc.Text
				var p fault.Problem
				if json.Unmarshal([]byte(tc.Text), &p) == nil && p.Code != "" {
					out.Problem = &p
				}
			}
		}
		return out, nil
	}
	raw, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(raw, &out.Out)
	return out, nil
}

func mustCall(t *testing.T, s *sdk.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	r, err := callTool(t, s, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if r.IsError {
		t.Fatalf("%s failed: %s", name, r.Text)
	}
	return r.Out
}

func mustFail(t *testing.T, s *sdk.ClientSession, name string, args map[string]any, code string) toolResult {
	t.Helper()
	r, err := callTool(t, s, name, args)
	if err != nil {
		t.Fatalf("%s: transport error %v", name, err)
	}
	if !r.IsError {
		t.Fatalf("%s succeeded, want error %q: %v", name, code, r.Out)
	}
	if code != "" && (r.Problem == nil || r.Problem.Code != code) {
		t.Fatalf("%s error %q, want code %q", name, r.Text, code)
	}
	return r
}

func TestMCPToolListingAndAnnotations(t *testing.T) {
	f := newMCPFixture(t)
	s := f.connect(f.dana.Access)
	if v := s.InitializeResult().ProtocolVersion; v != "2026-07-28" {
		t.Fatalf("negotiated protocol %q", v)
	}
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*sdk.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}
	reads := []string{"ticketsList", "ticketsSearch", "ticketsGet", "groupsList", "agentsList", "statusesList",
		"contactsList", "contactsGet", "cannedRepliesList", "tagsList", "holdReasonsList", "organisationsList", "organisationsGet"}
	want := append([]string{"ticketsCreate", "ticketsFollowUp", "ticketsCopy", "ticketsClaim", "ticketsReply", "ticketsAddNote", "ticketsUpdate",
		"ticketsTransition", "ticketsAssign", "contactsUpsert", "tagsCreate", "tagsDelete", "holdReasonsCreate", "holdReasonsUpdate", "holdReasonsDelete", "organisationsCreate", "organisationsUpdate", "organisationsDelete"}, reads...)
	if len(byName) != len(want) {
		t.Fatalf("tools: %v", byName)
	}
	for _, n := range want {
		tl := byName[n]
		if tl == nil || tl.Annotations == nil {
			t.Fatalf("tool %s missing or unannotated", n)
		}
		read := slices.Contains(reads, n)
		if tl.Annotations.ReadOnlyHint != read {
			t.Errorf("%s readOnlyHint = %v", n, tl.Annotations.ReadOnlyHint)
		}
		if !read && n != "tagsDelete" && n != "holdReasonsDelete" && n != "organisationsDelete" && (tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint) {
			t.Errorf("%s must declare destructiveHint false", n)
		}
	}
	for _, n := range []string{"tagsDelete", "holdReasonsDelete", "organisationsDelete"} {
		if d := byName[n].Annotations.DestructiveHint; d == nil || !*d {
			t.Errorf("%s must declare destructiveHint true", n)
		}
	}
	if !byName["ticketsUpdate"].Annotations.IdempotentHint {
		t.Error("ticketsUpdate should be idempotent")
	}
	for _, n := range []string{"ticketsAddNote", "ticketsReply"} {
		raw, _ := json.Marshal(byName[n].InputSchema)
		var sch struct {
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(raw, &sch)
		if !slices.Contains(sch.Required, "audience") || len(sch.Properties["audience"]["enum"].([]any)) != 1 {
			t.Errorf("%s schema must require a single-valued audience: %s", n, raw)
		}
	}
	for _, n := range []string{"delete", "restore", "settings"} {
		for name := range byName {
			if strings.Contains(name, n) && name != "tagsDelete" { // tagsDelete manages the catalog (tags:write), not tickets
				t.Errorf("unexpected tool %s", name)
			}
		}
	}
}

func TestMCPMatchesRESTAuthorization(t *testing.T) {
	f := newMCPFixture(t)
	_, tok := f.integration("MCP gateway", "tickets:read", "tickets:create", "tickets:replyAsRequester")

	// A ticket with a public message and an agent note; another in Delivery
	// (Dana is not a member of Delivery).
	tk := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.do("POST", tpath(tk, "/messages"), map[string]any{"visibility": "agent", "body": "SECRET-NOTE internal only"}, bearer(f.dana.Access)).expect(201)
	other := f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery, "subject": "Delivery only"}).Ref()

	ig := f.connect(tok)
	agent := f.connect(f.dana.Access)

	// Integrations: public content only, no assignee.
	got := mustCall(t, ig, "ticketsGet", map[string]any{"ticket": tk})
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET-NOTE") || strings.Contains(string(raw), `"visibility":"agent"`) {
		t.Fatalf("integration saw an agent note: %s", raw)
	}
	msgs := got["messages"].(map[string]any)["items"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("integration messages: %v", msgs)
	}
	rest := f.do("GET", tpath(tk, "/messages"), nil, bearer(tok)).expect(200).Items()
	if len(rest) != len(msgs) {
		t.Fatalf("MCP returned %d messages, REST %d", len(msgs), len(rest))
	}
	mustFail(t, ig, "ticketsAddNote", map[string]any{"ticket": tk, "audience": "agent", "body": "x"}, "insufficientScope")
	mustFail(t, ig, "ticketsClaim", map[string]any{"ticket": tk}, "userRequired")
	mustFail(t, ig, "ticketsUpdate", map[string]any{"ticket": tk, "priority": 1}, "userRequired")
	mustFail(t, ig, "ticketsList", map[string]any{"view": "myWork"}, "userRequired")

	// The agent sees notes, but not other groups' tickets
	// (exactly the REST answers).
	got = mustCall(t, agent, "ticketsGet", map[string]any{"ticket": tk})
	if raw, _ := json.Marshal(got); !strings.Contains(string(raw), "SECRET-NOTE") {
		t.Fatalf("agent should see its note: %s", raw)
	}
	mustFail(t, agent, "ticketsGet", map[string]any{"ticket": other}, "ticketNotFound")
	f.get(f.dana.Access, other).expect(404, "ticketNotFound")
	mustFail(t, agent, "ticketsClaim", map[string]any{"ticket": other}, "ticketNotFound")
	list := mustCall(t, agent, "ticketsList", map[string]any{"view": "all"})["items"].([]any)
	if len(list) != 1 || refOf(list[0].(map[string]any)) != tk {
		t.Fatalf("agent list: %v", list)
	}
	found := mustCall(t, agent, "ticketsSearch", map[string]any{"query": "Delivery"})["items"].([]any)
	if len(found) != 0 {
		t.Fatalf("search leaked another group's ticket: %v", found)
	}

	// Mutations go through the same services: claim, stale version,
	// priority, transition.
	claimed := mustCall(t, agent, "ticketsClaim", map[string]any{"ticket": tk})
	if claimed["assignee"].(map[string]any)["id"] != f.dana.UserId {
		t.Fatalf("claim: %v", claimed)
	}
	mustFail(t, agent, "ticketsClaim", map[string]any{"ticket": tk}, "ticketAlreadyClaimed")
	mustFail(t, agent, "ticketsUpdate", map[string]any{"ticket": tk, "priority": 1, "expectedVersion": 1}, "versionConflict")
	p1 := mustCall(t, agent, "ticketsUpdate", map[string]any{"ticket": tk, "priority": 1})
	if p1["priority"] != float64(1) {
		t.Fatalf("priority: %v", p1)
	}
	mustFail(t, agent, "ticketsUpdate", map[string]any{"ticket": tk, "priority": 5}, "")
	p1 = mustCall(t, agent, "ticketsUpdate", map[string]any{"ticket": tk, "ccEmails": []string{"Ops@Acme.example"}})
	if cc := p1["ccEmails"].([]any); len(cc) != 1 || cc[0] != "ops@acme.example" {
		t.Fatalf("cc: %v", p1)
	}
	mustFail(t, agent, "ticketsUpdate", map[string]any{"ticket": tk, "ccEmails": []string{"nope"}}, "")
	res := mustCall(t, agent, "ticketsTransition", map[string]any{"ticket": tk, "action": "resolve", "expectedVersion": p1["version"]})
	if obj(res, "status")["category"] != "resolved" {
		t.Fatalf("transition: %v", res)
	}

	// Requester reply through the integration reopens the resolved ticket.
	rep := mustCall(t, ig, "ticketsReply", map[string]any{"ticket": tk, "audience": "public", "body": "Still bouncing."})
	if obj(rep, "message")["visibility"] != "public" || obj(obj(rep, "ticket"), "status")["category"] == "resolved" {
		t.Fatalf("integration reply: %v", rep)
	}
	if _, ok := obj(rep, "ticket")["assignee"]; ok && obj(rep, "ticket")["assignee"] != nil {
		t.Fatalf("integration saw the assignee: %v", rep)
	}

	// Every tool call is audited without bodies.
	events, err := f.c.Audit.RecentAudit(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	var calls, failures int
	for _, ev := range events {
		if ev.Action != "mcp.toolCalled" {
			continue
		}
		calls++
		if ev.Outcome == "failure" {
			failures++
		}
		if !strings.HasPrefix(ev.Target, "mcpTool/") {
			t.Errorf("audit target %q", ev.Target)
		}
		for _, c := range ev.Changes {
			if strings.Contains(c, "SECRET") || strings.Contains(c, "bouncing") {
				t.Errorf("audit recorded a value: %v", ev.Changes)
			}
		}
	}
	if calls < 15 || failures < 7 {
		t.Fatalf("audit: %d calls, %d failures", calls, failures)
	}
}

func TestMCPNoteRequiresExplicitAudience(t *testing.T) {
	f := newMCPFixture(t)
	tk := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	s := f.connect(f.dana.Access)
	base := map[string]any{"ticket": tk, "body": "Checked the MX records."}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for i, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"ticketsAddNote", base, "audience"},                                       // missing audience
		{"ticketsAddNote", with(map[string]any{"audience": "public"}), "audience"}, // a note is never public
		{"ticketsAddNote", with(map[string]any{"audience": ""}), "audience"},       // empty
		{"ticketsReply", base, "audience"},                                         // replies are explicit too
		{"ticketsReply", with(map[string]any{"audience": "agent"}), "audience"},    // and cannot be notes
		{"ticketsAddNote", with(map[string]any{"audience": "agent", "x": 1}), "x"}, // unknown argument
	} {
		if r := mustFail(t, s, c.tool, c.args, ""); !strings.Contains(r.Text, c.want) {
			t.Errorf("case %d: error %q does not mention %q", i, r.Text, c.want)
		}
	}
	if n := len(f.do("GET", tpath(tk, "/messages"), nil, bearer(f.dana.Access)).expect(200).Items()); n != 1 {
		t.Fatalf("rejected calls created messages: %d", n)
	}
	out := mustCall(t, s, "ticketsAddNote", with(map[string]any{"audience": "agent"}))
	if obj(out, "message")["visibility"] != "agent" {
		t.Fatalf("note: %v", out)
	}
	pub := f.do("GET", tpath(tk, "/messages?visibility=public"), nil, bearer(f.dana.Access)).expect(200).Items()
	if len(pub) != 1 {
		t.Fatalf("the note became public: %v", pub)
	}
}

func TestMCPCredentialScopeRevocationAndOrigin(t *testing.T) {
	f := newMCPFixture(t)
	id, tok := f.integration("MCP client", "tickets:read")

	// Every token is admitted to /mcp (there is no separate MCP scope);
	// an unauthenticated request is not.
	f.do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).expect(401, "authenticationRequired")

	// MCP does not widen REST scopes: no tickets:create, no create.
	s := f.connect(tok)
	mustFail(t, s, "ticketsCreate", map[string]any{"subject": "x", "body": "y",
		"requester": map[string]any{"name": "P", "email": "p@acme.example"}}, "insufficientScope")
	mustCall(t, s, "ticketsList", nil)

	// Revocation applies to the very next call.
	f.do("DELETE", "/api/v1/integrations/"+id, nil, bearer(f.admin.Access)).expect(204)
	if r, err := callTool(t, s, "ticketsList", nil); err == nil && !r.IsError {
		t.Fatal("revoked credential still accepted")
	}
	f.do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, bearer(tok)).expect(401, "invalidToken")

	// Origin: absent passes; a foreign or same-Host (DNS rebinding) origin
	// is rejected before authentication; the configured base URL passes.
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	accept := header("Accept", "application/json, text/event-stream")
	f.do("POST", "/mcp", init, bearer(f.dana.Access), accept).expect(200)
	f.do("POST", "/mcp", init, bearer(f.dana.Access), accept, header("Origin", "https://evil.example")).expect(403, "originRejected")
	f.do("POST", "/mcp", init, accept, header("Origin", "https://evil.example")).expect(403, "originRejected")
	f.do("POST", "/mcp", init, bearer(f.dana.Access), accept, header("Origin", testOrigin)).expect(403, "originRejected")
	settings := f.do("GET", "/api/v1/settings", nil, bearer(f.admin.Access)).expect(200)
	doc := settings.JSON()
	delete(doc, "configVersion")
	if smtp, ok := doc["smtp"].(map[string]any); ok {
		delete(smtp, "passwordConfigured")
	}
	doc["product"].(map[string]any)["baseUrl"] = testOrigin
	f.do("PUT", "/api/v1/settings", doc, bearer(f.admin.Access), header("If-Match", jsonString(settings.JSON()["configVersion"]))).expect(200)
	if _, err := f.c.Settings.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.do("POST", "/mcp", init, bearer(f.dana.Access), accept, header("Origin", testOrigin)).expect(200)
	// Stateless: no session header and no GET stream.
	r := f.do("POST", "/mcp", init, bearer(f.dana.Access), accept)
	if r.Header.Get("Mcp-Session-Id") != "" {
		t.Fatal("stateless endpoint issued a session ID")
	}
	f.do("GET", "/mcp", nil, bearer(f.dana.Access), accept).expect(405)
}

func TestMCPLegacyProtocolAndCrossInstance(t *testing.T) {
	f := newMCPFixture(t)
	f.createTicket(f.dana.Access, map[string]any{})

	// A 2025-06-18 client: initialize and tools/call as independent POSTs.
	accept := header("Accept", "application/json, text/event-stream")
	ver := header("MCP-Protocol-Version", "2025-06-18")
	r := f.do("POST", "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ticketsList","arguments":{}}}`,
		bearer(f.dana.Access), accept, ver).expect(200)
	if !strings.Contains(string(r.Body), "number") {
		t.Fatalf("legacy call: %s", r.Body)
	}

	// A second process (own container and router, same database): calls
	// alternate between both instances without any session affinity.
	c2 := f.container()
	srv2 := httptest.NewServer(f.router(c2))
	t.Cleanup(srv2.Close)
	hosts := []string{strings.TrimPrefix(f.srv.URL, "http://"), strings.TrimPrefix(srv2.URL, "http://")}
	s, err := connectMCP(t, f.srv.URL+"/mcp", f.dana.Access, hosts...)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		items := mustCall(t, s, "ticketsList", map[string]any{})["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("call %d: %v", i, items)
		}
	}
}

func TestMCPCreateIdempotency(t *testing.T) {
	f := newMCPFixture(t)
	_, tok := f.integration("MCP intake", "tickets:read", "tickets:create")
	s := f.connect(tok)
	args := map[string]any{"subject": "Webmail login loop", "body": "Users are sent back to the login page.",
		"requester": map[string]any{"name": "Priya Raman", "email": "priya@acme.example"}, "priority": 2, "idempotencyKey": "intake-42"}
	first := mustCall(t, s, "ticketsCreate", args)
	second := mustCall(t, s, "ticketsCreate", args)
	if refOf(first) != "1" || refOf(second) != refOf(first) || first["source"] != "api" || first["requestedPriority"] != float64(2) {
		t.Fatalf("create replay: %v / %v", first, second)
	}
	changed := map[string]any{}
	for k, v := range args {
		changed[k] = v
	}
	changed["subject"] = "Different"
	mustFail(t, s, "ticketsCreate", changed, "idempotencyKeyReused")
	delete(changed, "idempotencyKey")
	mustCall(t, s, "ticketsCreate", changed)
	if n := len(f.do("GET", "/api/v1/tickets", nil, bearer(f.admin.Access)).expect(200).Items()); n != 2 {
		t.Fatalf("tickets: %d", n)
	}
	// The same key on REST is a separate key space (different operation).
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "REST", "body": "b",
		"requester": map[string]any{"name": "P", "email": "p@acme.example"}}, bearer(tok), header("Idempotency-Key", "intake-42")).expect(201)
}
