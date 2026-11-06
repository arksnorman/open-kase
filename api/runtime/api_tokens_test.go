package runtime

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	mongostore "github.com/arksnorman/open-kase/api/internal/persistence/mongo"
	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/package/logger"
)

// token creates an API token with scopes and returns its ID and token.
func (f *ticketFixture) token(name string, scopes ...string) (string, string) {
	f.t.Helper()
	r := f.do("POST", "/api/v1/integrations", map[string]any{"name": name, "scopes": scopes}, bearer(f.admin.Access)).expect(201)
	if _, ok := r.JSON()["type"]; ok {
		f.t.Fatalf("created token has a type: %s", r.Body)
	}
	return r.Str("id"), r.Str("token")
}

// tokenAudit returns the audit events of one principal, newest first.
func (f *ticketFixture) tokenAudit(id string) []mongostore.AuditEvent {
	f.t.Helper()
	all, err := f.c.Audit.RecentAudit(context.Background(), 2000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []mongostore.AuditEvent
	for _, ev := range all {
		if ev.ActorId == id {
			out = append(out, ev)
		}
	}
	return out
}

// TestAgentTokenScopes checks every scope: the operation it allows
// succeeds, and without it the same request is refused (403
// insufficientScope, or userRequired for staff routes when the token holds
// no staff scope). tickets:read is needed to address existing tickets.
func TestAgentTokenScopes(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.reply(f.dana.Access, key, "agent", "INTERNAL-ONLY staff note")
	denied := func(r result) { t.Helper(); r.expect(403, "insufficientScope") }

	// tickets:read alone: tickets and public messages, the requester-side
	// projection; no staff routes (no staff scope).
	staffOnly := func(r result) { t.Helper(); r.expect(403, "userRequired") }
	_, read := f.token("Reader bot", "tickets:read")
	f.do("GET", "/api/v1/tickets", nil, bearer(read)).expect(200)
	if got := f.get(read, key).expect(200).JSON(); got["version"] != float64(0) || got["sla"] != nil {
		t.Fatalf("a token without a staff scope should get the requester projection: %v", got)
	}
	if page := f.do("GET", tpath(key, "/messages"), nil, bearer(read)).expect(200); strings.Contains(string(page.Body), "INTERNAL-ONLY") {
		t.Fatal("tickets:read alone returned an agent note")
	}
	staffOnly(f.do("GET", "/api/v1/groups", nil, bearer(read)))
	staffOnly(f.do("GET", "/api/v1/members", nil, bearer(read)))
	staffOnly(f.do("GET", "/api/v1/macros", nil, bearer(read)))
	denied(f.do("POST", "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "R", "email": "r@x.example"}}, bearer(read)))
	denied(f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "hi"}, bearer(read)))
	denied(f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "hi"}, bearer(read)))
	staffOnly(f.do("PATCH", tpath(key), map[string]any{"priority": 1}, bearer(read)))
	staffOnly(f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(read), ifMatchV(f.version(key))))
	staffOnly(f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery}, bearer(read), ifMatchV(f.version(key))))
	denied(f.do("GET", "/api/v1/contacts", nil, bearer(read)))
	denied(f.do("POST", "/api/v1/contacts", map[string]any{"name": "C", "email": "c@x.example"}, bearer(read)))

	// tickets:create without tickets:read: can create, cannot read.
	_, create := f.token("Intake bot", "tickets:create")
	f.createTicket(create, map[string]any{"priority": 2})
	denied(f.do("GET", "/api/v1/tickets", nil, bearer(create)))
	f.do("GET", "/api/v1/groups", nil, bearer(create)).expect(403)

	// tickets:comment: public reply as the bot; with tickets:read the staff
	// projection, groups, agents and canned replies; still no notes.
	_, comment := f.token("Reply bot", "tickets:read", "tickets:comment")
	f.reply(comment, key, "public", "We are looking into it.")
	if got := f.get(comment, key).expect(200).JSON(); got["version"] == float64(0) || got["tags"] == nil {
		t.Fatalf("a staff token should get the staff projection: %v", got)
	}
	f.do("GET", "/api/v1/groups", nil, bearer(comment)).expect(200)
	f.do("GET", "/api/v1/members", nil, bearer(comment)).expect(200)
	f.do("GET", "/api/v1/macros", nil, bearer(comment)).expect(200)
	if page := f.do("GET", tpath(key, "/messages"), nil, bearer(comment)).expect(200); strings.Contains(string(page.Body), "INTERNAL-ONLY") {
		t.Fatal("tickets:comment returned an agent note")
	}
	denied(f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "x"}, bearer(comment)))
	denied(f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "x", "as": "requester"}, bearer(comment)))

	// tickets:internal: read and write agent notes; no public reply.
	_, internal := f.token("Triage bot", "tickets:read", "tickets:internal")
	f.reply(internal, key, "agent", "Likely the MX record.")
	if page := f.do("GET", tpath(key, "/messages"), nil, bearer(internal)).expect(200); !strings.Contains(string(page.Body), "INTERNAL-ONLY") {
		t.Fatal("tickets:internal did not return agent notes")
	}
	denied(f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "x"}, bearer(internal)))
	denied(f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "x", "thenStatusId": f.statusID("resolved")}, bearer(internal)))

	// tickets:write: priority, tags, status, group and assignee.
	_, write := f.token("Router bot", "tickets:read", "tickets:write")
	f.addTags(f.admin, "vip")
	f.do("PATCH", tpath(key), map[string]any{"subject": "x"}, bearer(write), ifMatchV(f.version(key))).expect(403, "subjectLocked")
	up := f.do("PATCH", tpath(key), map[string]any{"priority": 1, "tags": []string{"vip"}}, bearer(write), ifMatchV(f.version(key))).expect(200).JSON()
	if up["priority"] != float64(1) || len(up["tags"].([]any)) != 1 {
		t.Fatalf("update: %v", up)
	}
	f.do("PATCH", tpath(key), map[string]any{"priority": 2}, bearer(write)).expect(428)
	writeID := f.do("GET", "/api/v1/integrations", nil, bearer(f.admin.Access)).expect(200).Items()
	botID := ""
	for _, it := range writeID {
		if it["name"] == "Router bot" {
			botID = it["id"].(string)
		}
	}
	// The assignee must be an eligible agent; the bot itself never is.
	f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery, "assigneeId": botID}, bearer(write), ifMatchV(f.version(key))).expect(422)
	f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery, "assigneeId": f.dana.UserId}, bearer(write), ifMatchV(f.version(key))).expect(422)
	as := f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery, "assigneeId": f.marco.UserId}, bearer(write), ifMatchV(f.version(key))).expect(200).JSON()
	if obj(as, "group")["id"] != f.delivery || obj(as, "assignee")["id"] != f.marco.UserId {
		t.Fatalf("assign: %v", as)
	}
	res := f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(write), ifMatchV(f.version(key))).expect(200).JSON()
	if obj(res, "status")["category"] != "resolved" {
		t.Fatalf("resolve: %v", res)
	}
	denied(f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "x"}, bearer(write)))

	// contacts:read / contacts:write.
	_, cr := f.token("CRM reader", "contacts:read")
	f.do("GET", "/api/v1/contacts", nil, bearer(cr)).expect(200)
	denied(f.do("POST", "/api/v1/contacts", map[string]any{"name": "C", "email": "c@x.example"}, bearer(cr)))
	_, cw := f.token("CRM writer", "contacts:write")
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "C", "email": "c@x.example"}, bearer(cw)).expect(201)
	denied(f.do("GET", "/api/v1/contacts", nil, bearer(cw)))

	// tickets:all covers every tickets:* scope (and nothing else).
	_, all := f.token("Support bot", "tickets:all")
	k2 := f.createTicket(all, map[string]any{}).Ref()
	f.reply(all, k2, "public", "Hello from the bot")
	f.reply(all, k2, "agent", "Bot note")
	f.do("PATCH", tpath(k2), map[string]any{"priority": 2}, bearer(all), ifMatchV(f.version(k2))).expect(200)
	denied(f.do("GET", "/api/v1/contacts", nil, bearer(all)))

	// Never beyond a human agent: no claim, admin or management
	// operations, whatever the scopes.
	_, every := f.token("Everything bot", "tickets:all", "contacts:read", "contacts:write")
	v := f.version(k2)
	f.do("POST", tpath(k2, "/claim"), nil, bearer(every), ifMatchV(v)).expect(403, "userRequired")
	f.do("GET", "/api/v1/queue-counts", nil, bearer(every)).expect(403, "userRequired")
	f.do("GET", tpath(k2, "/activity"), nil, bearer(every)).expect(403, "userRequired")
	f.do("GET", "/api/v1/users", nil, bearer(every)).expect(403)
	f.do("GET", "/api/v1/settings", nil, bearer(every)).expect(403)
	f.do("POST", "/api/v1/groups", map[string]any{"name": "Bots"}, bearer(every)).expect(403)
	f.do("PUT", "/api/v1/groups/"+f.delivery+"/members/"+f.dana.UserId, nil, bearer(every)).expect(403)
	f.do("POST", "/api/v1/integrations", map[string]any{"name": "x", "scopes": []string{"tickets:all"}}, bearer(every)).expect(403)
	// tickets:all never speaks for the requester.
	denied(f.do("POST", tpath(k2, "/messages"), map[string]any{"visibility": "public", "body": "x", "as": "requester"}, bearer(every)))
	f.do("GET", "/api/v1/webhooks", nil, bearer(every)).expect(403)
	f.do("GET", "/api/v1/reports/summary", nil, bearer(every)).expect(403)
	f.do("POST", "/api/v1/macros", map[string]any{"name": "x", "body": "y", "visibility": "public"}, bearer(every)).expect(403)
	f.do("PUT", "/api/v1/sla", map[string]any{}, bearer(every)).expect(403)
	f.do("PATCH", "/api/v1/project", map[string]any{"requesterAcknowledgement": false}, bearer(every), ifMatchV(1)).expect(403)
	msgs := f.do("GET", tpath(k2, "/messages"), nil, bearer(every)).expect(200).Items()
	f.do("POST", tpath(k2, "/messages/"+msgs[0]["id"].(string)+"/redact"), map[string]any{"reason": "privacy"}, bearer(every)).expect(403, "userRequired")
	f.do("POST", tpath(k2, "/links"), map[string]any{"ticket": key, "type": "related"}, bearer(every)).expect(403, "userRequired")
	f.do("PUT", tpath(k2, "/watchers/me"), nil, bearer(every)).expect(403, "userRequired")
}

// statusID returns the ID of a built-in status by category.
func (f *ticketFixture) statusID(category string) string {
	f.t.Helper()
	for _, s := range f.do("GET", "/api/v1/statuses", nil, bearer(f.admin.Access)).expect(200).Items() {
		if s["category"] == category {
			return s["id"].(string)
		}
	}
	f.t.Fatalf("no %s status", category)
	return ""
}

// TestRequesterVersusAgentToken: a requester token acts for the requester
// (no notes, no staff data); an agent token acts as support staff under its
// own name: its reply is a staff reply (first response, SLA) shown as a bot.
func TestRequesterVersusAgentToken(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.integration, map[string]any{}).Ref() // requester token: as the contact
	f.reply(f.dana.Access, key, "agent", "INTERNAL-ONLY staff note")

	// Requester token: notes are neither readable nor writable; even a
	// requester token that holds every requester scope.
	_, req := f.token("Portal", "tickets:read", "tickets:create", "tickets:replyAsRequester", "contacts:read", "contacts:write")
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "x"}, bearer(req)).expect(403, "insufficientScope")
	f.do("GET", tpath(key, "/messages?visibility=agent"), nil, bearer(req)).expect(200)
	if page := f.do("GET", tpath(key, "/messages"), nil, bearer(req)).expect(200); strings.Contains(string(page.Body), "INTERNAL-ONLY") {
		t.Fatal("requester token read an agent note")
	}
	if got := f.get(req, key).expect(200).JSON(); got["version"] != float64(0) || got["firstResponseAt"] != nil {
		t.Fatalf("requester token got staff data: %v", got)
	}
	f.do("PATCH", tpath(key), map[string]any{"priority": 1}, bearer(req)).expect(403, "userRequired")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive"}, bearer(req), ifMatchV(1)).expect(403, "userRequired")
	f.do("GET", "/api/v1/groups", nil, bearer(req)).expect(403, "userRequired")
	f.do("GET", "/api/v1/macros", nil, bearer(req)).expect(403, "userRequired")
	// A requester reply is the contact's.
	r := f.reply(req, key, "public", "Any update?").JSON()
	if obj(r, "author")["context"] != "contact" {
		t.Fatalf("requester reply author: %v", r["author"])
	}

	// Agent token: the reply is the bot's and counts as the first staff
	// response.
	if f.get(f.admin.Access, key).expect(200).JSON()["firstResponseAt"] != nil {
		t.Fatal("first response before any staff reply")
	}
	botID, bot := f.token("Support bot", "tickets:read", "tickets:comment", "tickets:internal")
	rep := f.reply(bot, key, "public", "Fixed the MX record, please retry.").JSON()
	author := obj(rep, "author")
	if author["context"] != "bot" || author["name"] != "Support bot" || author["id"] != botID {
		t.Fatalf("bot reply author: %v", author)
	}
	if obj(rep, "ticket")["firstResponseAt"] == nil {
		t.Fatalf("a bot public reply is a staff reply: %v", rep["ticket"])
	}
	note := obj(f.reply(bot, key, "agent", "Checked DNS.").JSON(), "author")
	if note["context"] != "bot" {
		t.Fatalf("bot note author: %v", note)
	}
	// Staff see the bot by name and ID; the requester side by name only.
	for _, m := range f.do("GET", tpath(key, "/messages"), nil, bearer(f.dana.Access)).expect(200).Items() {
		if a := m["author"].(map[string]any); a["name"] == "Support bot" && (a["context"] != "bot" || a["id"] != botID) {
			t.Fatalf("staff view of the bot: %v", a)
		}
	}
	seen := false
	for _, m := range f.do("GET", tpath(key, "/messages"), nil, bearer(req)).expect(200).Items() {
		if a := m["author"].(map[string]any); a["name"] == "Support bot" {
			seen = true
			if a["context"] != "bot" || a["id"] != "" {
				t.Fatalf("requester view of the bot: %v", a)
			}
		}
	}
	if !seen {
		t.Fatal("requester does not see the bot's public reply")
	}
	// The activity timeline names the bot (type service) and its scope.
	var botEvents int
	for _, ev := range f.do("GET", tpath(key, "/activity?limit=100"), nil, bearer(f.admin.Access)).expect(200).Items() {
		a := ev["actor"].(map[string]any)
		if a["id"] == botID {
			botEvents++
			// The write scope, not tickets:read (needed to address the ticket).
			if want := map[string]string{"visibility:public": "tickets:comment", "visibility:agent": "tickets:internal"}[ev["changes"].([]any)[0].(string)]; a["type"] != "service" || a["name"] != "Support bot" || jsonString(a["scopes"]) != jsonString([]string{want}) {
				t.Fatalf("activity actor: %v (event %v)", a, ev)
			}
		}
	}
	if botEvents != 2 {
		t.Fatalf("bot activity events: %d", botEvents)
	}
}

// TestRESTAndMCPAnswersMatch: the MCP tools return exactly the REST
// representations, and refuse with the same problem codes.
func TestRESTAndMCPAnswersMatch(t *testing.T) {
	f := newMCPFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.reply(f.dana.Access, key, "agent", "INTERNAL-ONLY staff note")
	f.do("POST", "/api/v1/macros", map[string]any{"name": "Thanks", "body": "Thank you!", "visibility": "public"}, bearer(f.admin.Access)).expect(201)
	contact := f.do("POST", "/api/v1/contacts", map[string]any{"name": "Ada", "email": "ada@x.example"}, bearer(f.admin.Access)).expect(201).Str("id")
	_, tok := f.token("Support bot", "tickets:all", "contacts:read")
	s := f.connect(tok)

	same := func(name string, mcp any, rest []byte) {
		t.Helper()
		var want any
		_ = json.Unmarshal(rest, &want)
		raw, _ := json.Marshal(mcp)
		var got any
		_ = json.Unmarshal(raw, &got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: MCP %s\nREST %s", name, raw, rest)
		}
	}
	got := mustCall(t, s, "ticketsGet", map[string]any{"ticket": key})
	same("ticket", got["ticket"], f.get(tok, key).expect(200).Body)
	msgs := got["messages"].(map[string]any)
	same("messages", msgs, f.do("GET", tpath(key, "/messages"), nil, bearer(tok)).expect(200).Body)
	if !strings.Contains(jsonString(msgs), "INTERNAL-ONLY") {
		t.Fatal("tickets:internal token should see agent notes over MCP")
	}
	same("groups", mustCall(t, s, "groupsList", nil), f.do("GET", "/api/v1/groups", nil, bearer(tok)).expect(200).Body)
	same("agents", mustCall(t, s, "agentsList", nil), f.do("GET", "/api/v1/members", nil, bearer(tok)).expect(200).Body)
	same("statuses", mustCall(t, s, "statusesList", nil), f.do("GET", "/api/v1/statuses", nil, bearer(tok)).expect(200).Body)
	same("canned replies", mustCall(t, s, "cannedRepliesList", nil), f.do("GET", "/api/v1/macros", nil, bearer(tok)).expect(200).Body)
	same("contact", mustCall(t, s, "contactsGet", map[string]any{"contactId": contact}), f.do("GET", "/api/v1/contacts/"+contact, nil, bearer(tok)).expect(200).Body)
	same("contacts", mustCall(t, s, "contactsList", nil), f.do("GET", "/api/v1/contacts", nil, bearer(tok)).expect(200).Body)
	same("list", mustCall(t, s, "ticketsList", map[string]any{"view": "all"}), f.do("GET", "/api/v1/tickets?view=all", nil, bearer(tok)).expect(200).Body)

	// The same refusals on both transports.
	mustFail(t, s, "contactsUpsert", map[string]any{"name": "B", "email": "b@x.example"}, "insufficientScope")
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "B", "email": "b@x.example"}, bearer(tok)).expect(403, "insufficientScope")
	mustFail(t, s, "ticketsClaim", map[string]any{"ticket": key}, "userRequired")
	_, ro := f.token("Reader", "tickets:read", "tickets:comment")
	r := f.connect(ro)
	mustFail(t, r, "ticketsAddNote", map[string]any{"ticket": key, "audience": "agent", "body": "x"}, "insufficientScope")
	mustFail(t, r, "ticketsUpdate", map[string]any{"ticket": key, "priority": 1}, "insufficientScope")
	mustFail(t, r, "ticketsAssign", map[string]any{"ticket": key, "groupId": f.delivery}, "insufficientScope")
	mustFail(t, r, "ticketsTransition", map[string]any{"ticket": key, "action": "resolve"}, "insufficientScope")
	if raw := jsonString(mustCall(t, r, "ticketsGet", map[string]any{"ticket": key})); strings.Contains(raw, "INTERNAL-ONLY") {
		t.Fatal("a token without tickets:internal saw an agent note over MCP")
	}
	_, req := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	q := f.connect(req)
	mustFail(t, q, "ticketsAddNote", map[string]any{"ticket": key, "audience": "agent", "body": "x"}, "insufficientScope")
	mustFail(t, q, "groupsList", nil, "userRequired")
	same("requester ticket", mustCall(t, q, "ticketsGet", map[string]any{"ticket": key, "includeMessages": false})["ticket"], f.get(req, key).expect(200).Body)

	// MCP writes behave as REST: the bot assigns to an eligible agent.
	v := f.version(key)
	mustFail(t, s, "ticketsAssign", map[string]any{"ticket": key, "groupId": f.delivery, "assigneeId": f.dana.UserId}, "validationFailed")
	as := mustCall(t, s, "ticketsAssign", map[string]any{"ticket": key, "groupId": f.delivery, "assigneeId": f.marco.UserId, "expectedVersion": v})
	if obj(as, "assignee")["id"] != f.marco.UserId {
		t.Fatalf("assign: %v", as)
	}
	un := mustCall(t, s, "ticketsAssign", map[string]any{"ticket": key, "groupId": f.delivery, "unassign": true})
	if un["assignee"] != nil {
		t.Fatalf("unassign: %v", un)
	}
}

// TestTokenWriteAuditSweep walks every state-changing REST route and every
// mutating MCP tool with API tokens. Each write a token can make must leave
// an audit record naming the token (ID and name) and the scopes that
// allowed it, without message bodies; every other route must refuse.
func TestTokenWriteAuditSweep(t *testing.T) {
	// The sweep sends a few hundred requests; rate limits are covered by
	// TestRateLimits.
	f := &mcpFixture{ticketFixture: newTicketFixture(t)}
	off := map[string]transporthttp.RateLimit{}
	for class := range transporthttp.DefaultRateLimits {
		off[class] = transporthttp.RateLimit{PerMinute: -1}
	}
	r, err := transporthttp.NewRouter(transporthttp.Deps{Logger: logger.Discard(), Readiness: transporthttp.NewReadiness(logger.Discard()),
		Services: f.c.httpServices(), MCP: f.c.mcpHandler(), RateLimits: off})
	if err != nil {
		t.Fatal(err)
	}
	f.h = r
	f.srv = httptest.NewServer(f.h)
	t.Cleanup(f.srv.Close)
	agentID, agent := f.token("Sweep bot", "tickets:all", "contacts:read", "contacts:write", "tags:write", "holdReasons:write", "organisations:write")
	reqID, req := f.token("Sweep portal", "tickets:read", "tickets:create", "tickets:replyAsRequester", "contacts:read", "contacts:write")
	const secret = "BODY-SECRET-TEXT"
	ticketBody := map[string]any{"subject": "Sweep", "body": secret, "requester": map[string]any{"name": "Rae", "email": "rae@x.example"}}

	// check asserts that the principal's audit events grew and that every
	// new one names the token and its scopes, never the body.
	check := func(what, id, name string, before int, wantActions ...string) {
		t.Helper()
		evs := f.tokenAudit(id)
		if len(evs) <= before {
			t.Fatalf("%s: no audit record", what)
		}
		fresh := evs[:len(evs)-before]
		var actions []string
		for _, ev := range fresh {
			actions = append(actions, ev.Action)
			if ev.ActorType != "service" || ev.ActorLabel != name {
				t.Errorf("%s: %s actor %s/%q", what, ev.Action, ev.ActorType, ev.ActorLabel)
			}
			if len(ev.ActorScopes) == 0 && ev.Outcome == "success" {
				t.Errorf("%s: %s records no scope", what, ev.Action)
			}
			// A business write names the write scope that allowed it, not
			// only the read scope used to address the ticket.
			if ev.Action != "mcp.toolCalled" && slices.Equal(ev.ActorScopes, []string{"tickets:read"}) {
				t.Errorf("%s: %s names only tickets:read", what, ev.Action)
			}
			if strings.Contains(jsonString(ev), secret) {
				t.Errorf("%s: %s leaks the body", what, ev.Action)
			}
		}
		for _, a := range wantActions {
			if !slices.Contains(actions, a) {
				t.Errorf("%s: actions %v, want %s", what, actions, a)
			}
		}
	}

	// ---- REST ----
	type step struct {
		token, id, name string
		body            any
		status          int
		actions         []string
		ifMatch         bool
		path            string // concrete path (default: route with :t = a fresh staff ticket)
		version         int64  // If-Match for a concrete path
		upload          bool
	}
	fresh := func() string { return f.createTicket(f.dana.Access, map[string]any{}).Ref() }
	f.addTags(f.admin, "sweep", "mcp", "sweep-del")
	for _, name := range []string{"Sweep reason", "Sweep delete"} {
		f.do("POST", "/api/v1/hold-reasons", map[string]any{"name": name, "maxDays": 2}, bearer(f.admin.Access)).expect(201)
	}
	for _, name := range []string{"Sweep org", "Sweep org delete"} {
		f.createOrg(f.admin.Access, name, "")
	}
	orgID := func(name string) string {
		for _, o := range f.do("GET", "/api/v1/organisations?q="+strings.ReplaceAll(name, " ", "%20"), nil, bearer(f.admin.Access)).expect(200).Items() {
			if o["name"] == name {
				return o["id"].(string)
			}
		}
		f.t.Fatalf("no organisation %q", name)
		return ""
	}
	ownKey := f.createTicket(req, map[string]any{}).Ref() // created by the requester token (subject change)
	steps := map[string][]step{
		"POST /api/v1/tickets": {
			{token: agent, id: agentID, name: "Sweep bot", body: ticketBody, status: 201, actions: []string{"ticket.created"}},
			{token: req, id: reqID, name: "Sweep portal", body: ticketBody, status: 201, actions: []string{"ticket.created"}},
		},
		"PATCH /api/v1/tickets/:t": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"priority": 1, "tags": []string{"sweep"}}, status: 200, ifMatch: true, actions: []string{"ticket.updated"}},
			{token: req, id: reqID, name: "Sweep portal", body: map[string]any{"subject": "Renamed by the portal"}, status: 200, path: tpath(ownKey), actions: []string{"ticket.updated"}},
		},
		"POST /api/v1/tickets/:t/assignments": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"groupId": f.delivery, "assigneeId": f.marco.UserId}, status: 200, ifMatch: true, actions: []string{"ticket.transferred"}},
		},
		"POST /api/v1/tickets/:t/transitions": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"action": "resolve"}, status: 200, ifMatch: true, actions: []string{"ticket.resolved"}},
		},
		"POST /api/v1/tickets/:t/messages": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"visibility": "public", "body": secret}, status: 201, actions: []string{"message.created"}},
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"visibility": "agent", "body": secret, "thenStatusId": f.statusID("resolved")}, status: 201, actions: []string{"message.created", "ticket.resolved"}},
			{token: req, id: reqID, name: "Sweep portal", body: map[string]any{"visibility": "public", "body": secret}, status: 201, actions: []string{"message.created"}},
		},
		"POST /api/v1/attachments": {
			{token: agent, id: agentID, name: "Sweep bot", upload: true, status: 201, actions: []string{"attachment.uploaded"}},
			{token: req, id: reqID, name: "Sweep portal", upload: true, status: 201, actions: []string{"attachment.uploaded"}},
		},
		"POST /api/v1/tickets/:t/follow-up": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"body": secret}, status: 201, actions: []string{"ticket.created", "ticket.linkAdded"}},
			{token: req, id: reqID, name: "Sweep portal", body: map[string]any{}, status: 201, actions: []string{"ticket.created"}},
		},
		"POST /api/v1/tickets/:t/copy": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"body": secret}, status: 201, actions: []string{"ticket.created", "ticket.linkAdded", "ticket.copied"}},
		},
		"POST /api/v1/tags": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"name": "sweep-new"}, status: 201, actions: []string{"tag.created"}},
		},
		"DELETE /api/v1/tags/:name": {
			{token: agent, id: agentID, name: "Sweep bot", status: 204, path: "/api/v1/tags/sweep-del", actions: []string{"tag.deleted"}},
		},
		"POST /api/v1/hold-reasons": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"name": "Sweep created", "maxDays": 2}, status: 201, actions: []string{"holdReason.created"}},
		},
		"PATCH /api/v1/hold-reasons/:h": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"maxDays": 4}, status: 200, path: "/api/v1/hold-reasons/" + f.holdReasonID("Sweep reason"),
				actions: []string{"holdReason.updated"}, version: 1},
		},
		"DELETE /api/v1/hold-reasons/:h": {
			{token: agent, id: agentID, name: "Sweep bot", status: 204, path: "/api/v1/hold-reasons/" + f.holdReasonID("Sweep delete"), version: 1, actions: []string{"holdReason.deleted"}},
		},
		"POST /api/v1/organisations": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"name": "Sweep created org"}, status: 201, actions: []string{"organisation.created"}},
		},
		"PATCH /api/v1/organisations/:o": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"externalRef": "SWEEP-1"}, status: 200, path: "/api/v1/organisations/" + orgID("Sweep org"),
				actions: []string{"organisation.updated"}, version: 1},
		},
		"DELETE /api/v1/organisations/:o": {
			{token: agent, id: agentID, name: "Sweep bot", status: 204, path: "/api/v1/organisations/" + orgID("Sweep org delete"), version: 1, actions: []string{"organisation.deleted"}},
		},
		"POST /api/v1/contacts": {
			{token: agent, id: agentID, name: "Sweep bot", body: map[string]any{"name": "Kim", "email": "kim@x.example"}, status: 201, actions: []string{"contact.created"}},
			{token: req, id: reqID, name: "Sweep portal", body: map[string]any{"name": "Kim Lee", "email": "kim@x.example"}, status: 200, actions: []string{"contact.updated"}},
		},
	}
	params := map[string]string{":msg": "000000000000000000000001", ":s": "000000000000000000000001", ":l": "000000000000000000000001",
		":g": f.delivery, ":u": f.dana.UserId, ":i": agentID, ":c": "000000000000000000000001", ":w": "000000000000000000000001",
		":d": "000000000000000000000001", ":m": "000000000000000000000001", ":visibility": "public", ":a": "000000000000000000000001"}
	walked := map[string]bool{}
	for _, rt := range f.h.(*gin.Engine).Routes() {
		if rt.Method == "GET" || !strings.HasPrefix(rt.Path, "/api/v1/") || strings.HasPrefix(rt.Path, "/api/v1/auth/") {
			continue
		}
		route := rt.Method + " " + rt.Path
		walked[route] = true
		if list, ok := steps[route]; ok {
			for _, st := range list {
				before := len(f.tokenAudit(st.id))
				var res result
				if st.upload {
					res = f.upload(st.token, "sweep.txt", []byte("sweep file"))
				} else {
					path := st.path
					opts := []reqOpt{bearer(st.token)}
					if st.version > 0 {
						opts = append(opts, ifMatchV(st.version))
					}
					if path == "" {
						key := fresh()
						path = strings.ReplaceAll(rt.Path, ":t", key)
						if st.ifMatch {
							opts = append(opts, ifMatchV(f.version(key)))
						}
					}
					res = f.do(rt.Method, path, st.body, opts...)
				}
				if res.Code != st.status {
					t.Fatalf("%s as %s: status %d, want %d: %s", route, st.name, res.Code, st.status, res.Body)
				}
				check(route+" as "+st.name, st.id, st.name, before, st.actions...)
			}
			continue
		}
		// Any other write route refuses both tokens.
		path := strings.ReplaceAll(rt.Path, ":t", fresh())
		for p, v := range params {
			path = strings.ReplaceAll(path, p, v)
		}
		for _, tok := range []string{agent, req} {
			res := f.do(rt.Method, path, map[string]any{}, bearer(tok), ifMatchV(1))
			if res.Code != 403 && res.Code != 404 {
				t.Errorf("%s: status %d, want a refusal (403/404); token-writable routes belong in the audit sweep: %s", route, res.Code, res.Body)
			}
		}
	}
	t.Logf("walked %d write routes", len(walked))
	for route := range steps {
		if !walked[route] {
			t.Errorf("sweep step for unknown route %s", route)
		}
	}

	// ---- MCP ----
	s := f.connect(agent)
	q := f.connect(req)
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type call struct {
		session  *sdk.ClientSession
		id, name string
		args     func() map[string]any
		actions  []string
		fails    string
	}
	email, tagN := 0, 0
	calls := map[string][]call{
		"ticketsCreate": {
			{s, agentID, "Sweep bot", func() map[string]any { return ticketBody }, []string{"ticket.created", "mcp.toolCalled"}, ""},
			{q, reqID, "Sweep portal", func() map[string]any { return ticketBody }, []string{"ticket.created", "mcp.toolCalled"}, ""},
		},
		"ticketsFollowUp": {
			{s, agentID, "Sweep bot", func() map[string]any { return map[string]any{"ticket": fresh(), "body": secret} }, []string{"ticket.created", "mcp.toolCalled"}, ""},
			{q, reqID, "Sweep portal", func() map[string]any { return map[string]any{"ticket": fresh()} }, []string{"ticket.created", "mcp.toolCalled"}, ""},
		},
		"ticketsCopy": {
			{s, agentID, "Sweep bot", func() map[string]any { return map[string]any{"ticket": fresh(), "body": secret} }, []string{"ticket.created", "ticket.copied", "mcp.toolCalled"}, ""},
			{q, reqID, "Sweep portal", func() map[string]any { return map[string]any{"ticket": fresh()} }, nil, "staffOnly"},
		},
		"ticketsReply": {
			{s, agentID, "Sweep bot", func() map[string]any { return map[string]any{"ticket": fresh(), "audience": "public", "body": secret} }, []string{"message.created", "mcp.toolCalled"}, ""},
			{q, reqID, "Sweep portal", func() map[string]any { return map[string]any{"ticket": fresh(), "audience": "public", "body": secret} }, []string{"message.created", "mcp.toolCalled"}, ""},
		},
		"ticketsAddNote": {
			{s, agentID, "Sweep bot", func() map[string]any { return map[string]any{"ticket": fresh(), "audience": "agent", "body": secret} }, []string{"message.created", "mcp.toolCalled"}, ""},
		},
		"ticketsUpdate": {
			{s, agentID, "Sweep bot", func() map[string]any {
				return map[string]any{"ticket": fresh(), "priority": 2, "tags": []string{"mcp"}}
			}, []string{"ticket.updated", "mcp.toolCalled"}, ""},
			{q, reqID, "Sweep portal", func() map[string]any { return map[string]any{"ticket": ownKey, "subject": "Renamed over MCP"} }, []string{"ticket.updated", "mcp.toolCalled"}, ""},
		},
		"ticketsTransition": {
			{s, agentID, "Sweep bot", func() map[string]any { return map[string]any{"ticket": fresh(), "action": "resolve"} }, []string{"ticket.resolved", "mcp.toolCalled"}, ""},
		},
		"ticketsAssign": {
			{s, agentID, "Sweep bot", func() map[string]any {
				return map[string]any{"ticket": fresh(), "groupId": f.delivery, "assigneeId": f.marco.UserId}
			}, []string{"ticket.transferred", "mcp.toolCalled"}, ""},
		},
		"contactsUpsert": {
			{s, agentID, "Sweep bot", func() map[string]any {
				email++
				return map[string]any{"name": "Mo", "email": "mo" + jsonString(email) + "@x.example"}
			}, []string{"contact.created", "mcp.toolCalled"}, ""},
		},
		"tagsCreate": {
			{s, agentID, "Sweep bot", func() map[string]any {
				tagN++
				return map[string]any{"name": "mcp-new-" + jsonString(tagN)}
			}, []string{"tag.created", "mcp.toolCalled"}, ""},
		},
		"organisationsCreate": {
			{s, agentID, "Sweep bot", func() map[string]any {
				tagN++
				return map[string]any{"name": "mcp org " + jsonString(tagN)}
			}, []string{"organisation.created", "mcp.toolCalled"}, ""},
		},
		"organisationsUpdate": {
			{s, agentID, "Sweep bot", func() map[string]any {
				return map[string]any{"organisationId": orgID("Sweep org"), "externalRef": "MCP-1"}
			}, []string{"organisation.updated", "mcp.toolCalled"}, ""},
		},
		"organisationsDelete": {
			{s, agentID, "Sweep bot", func() map[string]any {
				tagN++
				name := "mcp del org " + jsonString(tagN)
				f.createOrg(f.admin.Access, name, "")
				return map[string]any{"organisationId": orgID(name)}
			}, []string{"organisation.deleted", "mcp.toolCalled"}, ""},
		},
		"holdReasonsCreate": {
			{s, agentID, "Sweep bot", func() map[string]any {
				tagN++
				return map[string]any{"name": "mcp reason " + jsonString(tagN), "maxDays": 3}
			}, []string{"holdReason.created", "mcp.toolCalled"}, ""},
		},
		"holdReasonsUpdate": {
			{s, agentID, "Sweep bot", func() map[string]any {
				return map[string]any{"holdReasonId": f.holdReasonID("Waiting on Engineering"), "maxDays": 6}
			}, []string{"holdReason.updated", "mcp.toolCalled"}, ""},
		},
		"holdReasonsDelete": {
			{s, agentID, "Sweep bot", func() map[string]any {
				tagN++
				name := "mcp del reason " + jsonString(tagN)
				f.do("POST", "/api/v1/hold-reasons", map[string]any{"name": name, "maxDays": 2}, bearer(f.admin.Access)).expect(201)
				return map[string]any{"holdReasonId": f.holdReasonID(name)}
			}, []string{"holdReason.deleted", "mcp.toolCalled"}, ""},
		},
		"tagsDelete": {
			{s, agentID, "Sweep bot", func() map[string]any {
				tagN++
				name := "mcp-del-" + jsonString(tagN)
				f.addTags(f.admin, name)
				return map[string]any{"name": name}
			}, []string{"tag.deleted", "mcp.toolCalled"}, ""},
		},
		// Claiming is for users only: refused, and the refusal is audited.
		"ticketsClaim": {
			{s, agentID, "Sweep bot", func() map[string]any { return map[string]any{"ticket": fresh()} }, []string{"mcp.toolCalled"}, "userRequired"},
		},
	}
	for _, tl := range tools.Tools {
		if tl.Annotations != nil && tl.Annotations.ReadOnlyHint {
			continue
		}
		list, ok := calls[tl.Name]
		if !ok {
			t.Errorf("mutating MCP tool %s is not in the audit sweep", tl.Name)
			continue
		}
		for _, c := range list {
			before := len(f.tokenAudit(c.id))
			if c.fails != "" {
				mustFail(t, c.session, tl.Name, c.args(), c.fails)
			} else {
				mustCall(t, c.session, tl.Name, c.args())
			}
			check("MCP "+tl.Name+" as "+c.name, c.id, c.name, before, c.actions...)
		}
	}
}

// TestBotMessagesInEmails: a bot's public reply is mailed to the requester
// like a staff reply, under the bot's name; agent-note notices name the bot.
func TestBotMessagesInEmails(t *testing.T) {
	f := newNotifyFixture(t)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.watch(f.marco.Access, key)
	f.drain()
	f.mail.Reset()
	_, bot := f.token("Support bot", "tickets:read", "tickets:comment", "tickets:internal")
	f.reply(bot, key, "public", "The MX record is fixed.")
	f.drain()
	got := sentTo(f.mail, "priya@acme.example")
	// Requester emails carry no message content (owner decision 2026-10-31).
	if len(got) != 1 || strings.Contains(got[0].Text+got[0].HTML, "The MX record is fixed.") || !strings.Contains(got[0].Text, "You received a response") {
		t.Fatalf("bot reply email to the requester: %+v", got)
	}
	if w := sentTo(f.mail, "marco@example.com"); len(w) != 1 || !strings.Contains(w[0].Text, "The MX record is fixed.") || !strings.Contains(w[0].Text, "Support bot") {
		t.Fatalf("watcher email about the bot reply: %+v", w)
	}
	f.mail.Reset()
	f.reply(bot, key, "agent", "BOT-SECRET checked DNS")
	f.drain()
	if len(sentTo(f.mail, "priya@acme.example")) != 0 {
		t.Fatal("requester got an agent-note email")
	}
	note := sentTo(f.mail, "marco@example.com")
	if len(note) != 1 || strings.Contains(note[0].Text+note[0].HTML, "BOT-SECRET") || !strings.Contains(note[0].Text, "Support bot (bot)") {
		t.Fatalf("agent-note notice: %+v", note)
	}
}
