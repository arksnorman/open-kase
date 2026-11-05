package runtime

import (
	"testing"
)

// TestAuditCatalogs covers the standard action and actor catalogs (owner
// request 2026-10-31): GET /audit-actions, GET /audit-actors, the exact
// action and {type, id} actor filters of GET /audit-events and the
// server-side action label.
func TestAuditCatalogs(t *testing.T) {
	f := newTicketFixture(t)
	tk := f.createTicket(f.admin.Access, map[string]any{})
	f.reply(f.dana.Access, tk.Str("id"), "agent", "catalog note")
	botID, bot := f.token("Catalog bot", "tickets:all")

	// Action catalog: codes, labels, categories.
	actions := map[string]map[string]any{}
	for _, it := range f.do("GET", "/api/v1/audit-actions", nil, bearer(f.admin.Access)).expect(200).Items() {
		actions[it["code"].(string)] = it
	}
	if a := actions["ticket.created"]; a == nil || a["label"] != "Created a ticket" || a["category"] != "Tickets" {
		t.Fatalf("ticket.created: %v", a)
	}
	if _, ok := actions["ticket.deleted"]; ok {
		t.Fatal("ticket.deleted is not written and must not be catalogued")
	}

	// Every listed event carries the catalog label.
	for _, it := range f.do("GET", "/api/v1/audit-events?limit=100", nil, bearer(f.admin.Access)).expect(200).Items() {
		a := actions[it["action"].(string)]
		if a == nil || it["actionLabel"] != a["label"] {
			t.Fatalf("event %v: label %v, catalog %v", it["action"], it["actionLabel"], a)
		}
	}

	// Exact action filter: "message.created" only; "message" (no dot) is an
	// exact code that matches nothing; "message." is a prefix.
	exact := f.do("GET", "/api/v1/audit-events?action=message.created", nil, bearer(f.admin.Access)).expect(200).Items()
	if len(exact) == 0 {
		t.Fatal("no message.created events")
	}
	for _, it := range exact {
		if it["action"] != "message.created" {
			t.Fatalf("exact filter: %v", it["action"])
		}
	}
	if n := len(f.do("GET", "/api/v1/audit-events?action=message", nil, bearer(f.admin.Access)).expect(200).Items()); n != 0 {
		t.Fatalf("action=message matched %d events", n)
	}

	// Actor catalog: users (with username/email detail), tokens, system actors.
	cat := f.do("GET", "/api/v1/audit-actors", nil, bearer(f.admin.Access)).expect(200).JSON()
	byKey := map[string]map[string]any{}
	order := []string{}
	for _, raw := range cat["items"].([]any) {
		it := raw.(map[string]any)
		byKey[it["type"].(string)+":"+it["id"].(string)] = it
		order = append(order, it["type"].(string))
	}
	if u := byKey["user:"+f.dana.UserId]; u == nil || u["name"] != "Dana Okafor" || u["detail"] == "" {
		t.Fatalf("dana: %v", u)
	}
	if b := byKey["token:"+botID]; b == nil || b["name"] != "Catalog bot" || b["detail"] != "API token" {
		t.Fatalf("bot: %v", b)
	}
	if s := byKey["system:automation"]; s == nil || s["name"] != "System · Automation" {
		t.Fatalf("system automation: %v", s)
	}
	for i := 1; i < len(order); i++ {
		rank := map[string]int{"user": 0, "token": 1, "system": 2}
		if rank[order[i]] < rank[order[i-1]] {
			t.Fatalf("actor order: %v", order)
		}
	}
	if cat["truncated"] != false {
		t.Fatalf("truncated: %v", cat["truncated"])
	}
	// Search narrows every kind; regex characters are literal.
	for _, raw := range f.do("GET", "/api/v1/audit-actors?q=okafor", nil, bearer(f.admin.Access)).expect(200).JSON()["items"].([]any) {
		if it := raw.(map[string]any); it["id"] != f.dana.UserId {
			t.Fatalf("q=okafor: %v", it)
		}
	}
	if items := f.do("GET", "/api/v1/audit-actors?q=.%2A", nil, bearer(f.admin.Access)).expect(200).JSON()["items"].([]any); len(items) != 0 {
		t.Fatalf("q=.*: %v", items)
	}
	if r := f.do("GET", "/api/v1/audit-actors?limit=1", nil, bearer(f.admin.Access)).expect(200).JSON(); r["truncated"] != true {
		t.Fatalf("limit=1 not truncated: %v", r)
	}
	f.do("GET", "/api/v1/audit-actors?limit=0", nil, bearer(f.admin.Access)).expect(422)
	f.do("GET", "/api/v1/audit-actors?limit=abc", nil, bearer(f.admin.Access)).expect(422)

	// Actor filter by {type, id}: a token, and a system actor kind.
	f.do("POST", tpath(tk.Str("id"), "/messages"), map[string]any{"visibility": "agent", "body": "bot note"}, bearer(bot)).expect(201)
	toks := f.do("GET", "/api/v1/audit-events?actorType=token&actorId="+botID, nil, bearer(f.admin.Access)).expect(200).Items()
	if len(toks) == 0 {
		t.Fatal("no events of the bot")
	}
	inst := f.do("GET", "/api/v1/audit-events?actorType=system&actorId=installation", nil, bearer(f.admin.Access)).expect(200).Items()
	if len(inst) == 0 {
		t.Fatal("no installation events")
	}
	for _, it := range inst {
		if a := it["actor"].(map[string]any); a["id"] != "installation" || a["name"] != "System · Installation" {
			t.Fatalf("installation actor: %v", a)
		}
	}
	f.do("GET", "/api/v1/audit-events?actorType=system&actorId=robots", nil, bearer(f.admin.Access)).expect(422)
	f.do("GET", "/api/v1/audit-events?actorType=user&actorId=installation", nil, bearer(f.admin.Access)).expect(422)

	// Admins only.
	for _, path := range []string{"/api/v1/audit-actions", "/api/v1/audit-actors"} {
		f.do("GET", path, nil, bearer(f.dana.Access)).expect(403)
		f.do("GET", path, nil, bearer(bot)).expect(403)
		f.do("GET", path, nil).expect(401)
	}
}
