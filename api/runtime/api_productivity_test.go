package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestTagValidation(t *testing.T) {
	f := newTicketFixture(t)
	f.createTicket(f.dana.Access, map[string]any{})
	f.addTags(f.admin, "dns-outage", "vip")
	patch := func(tok string, body map[string]any) result {
		return f.do("PATCH", tpath("1"), body, bearer(tok), ifMatchV(f.version("1")))
	}

	// Tags are normalized, deduplicated and sorted; staff-only data.
	v := f.version("1")
	tk := patch(f.dana.Access, map[string]any{"tags": []string{"  DNS Outage ", "vip", "dns-outage"}}).expect(200).JSON()
	if got := tk["tags"].([]any); len(got) != 2 || got[0] != "dns-outage" || got[1] != "vip" {
		t.Fatalf("tags %v", got)
	}
	if tk["version"] != float64(v+1) {
		t.Fatalf("tag change must bump the version: %v", tk["version"])
	}
	if r := patch(f.dana.Access, map[string]any{"tags": []string{"#bad"}}).expect(422); r.Problem().Errors[0].Field != "tags[0]" {
		t.Fatalf("bad tag error %s", r.Body)
	}
	many := []string{}
	for i := range 21 {
		many = append(many, "t"+strings.Repeat("x", i))
	}
	patch(f.dana.Access, map[string]any{"tags": many}).expect(422)
	// Tags never reach the requester side and do not move its update time.
	pub := f.get(f.integration, "1").expect(200).JSON()
	if len(pub["tags"].([]any)) != 0 {
		t.Fatalf("integration sees staff data %v", pub)
	}
	if n := f.outboxCount(t, "ticket.updated"); n != 0 {
		t.Fatalf("tag-only change emitted %d ticket.updated events", n)
	}
	tags := f.do("GET", "/api/v1/tags", nil, bearer(f.dana.Access)).expect(200).Items()
	if len(tags) != 2 || tags[0]["count"] != float64(1) {
		t.Fatalf("tag list %v", tags)
	}
	f.do("GET", "/api/v1/tags", nil, bearer(f.integration)).expect(403, "insufficientScope")
}

func TestListFiltersNeverWidenAccess(t *testing.T) {
	f := newTicketFixture(t)
	// #1 in Triage (Dana and Marco), #2 in Delivery (Marco only).
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	f.createTicket(f.marco.Access, map[string]any{"priority": 1, "groupId": f.delivery, "subject": "Delivery issue"})
	f.addTags(f.admin, "vip")
	f.do("PATCH", tpath("2"), map[string]any{"tags": []string{"vip"}}, bearer(f.marco.Access), ifMatchV(f.version("2"))).expect(200)

	// Filters are intersected with access: Dana cannot read Delivery tickets.
	if got := f.do("GET", "/api/v1/tickets?tag=vip", nil, bearer(f.dana.Access)).expect(200).Items(); len(got) != 0 {
		t.Fatalf("tag filter widened access: %v", keys(got))
	}
	if got := keys(f.do("GET", "/api/v1/tickets?tag=vip", nil, bearer(f.marco.Access)).expect(200).Items()); len(got) != 1 || got[0] != "2" {
		t.Fatalf("tag filter for marco %v", got)
	}
	path := "/api/v1/tickets?priority=1"
	if got := keys(f.do("GET", path, nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 1 || got[0] != "1" {
		t.Fatalf("dana with priority filter %v", got)
	}
	if got := keys(f.do("GET", path, nil, bearer(f.marco.Access)).expect(200).Items()); len(got) != 2 {
		t.Fatalf("marco with priority filter %v", got)
	}
	// Integrations cannot use filters.
	f.do("GET", path, nil, bearer(f.integration)).expect(403, "userRequired")
	// Validation: bounded, typed filters; no combination with other views.
	f.do("GET", "/api/v1/tickets?priority=0", nil, bearer(f.dana.Access)).expect(422)
	f.do("GET", "/api/v1/tickets?assignee=someone", nil, bearer(f.dana.Access)).expect(422)
	f.do("GET", "/api/v1/tickets?due=tomorrow", nil, bearer(f.dana.Access)).expect(422)
	f.do("GET", path+"&view=deleted", nil, bearer(f.admin.Access)).expect(422)
	f.do("GET", "/api/v1/tickets?priority=9", nil, bearer(f.dana.Access)).expect(422)
	// "me" resolves to the viewer.
	f.do("POST", tpath("1", "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	if got := keys(f.do("GET", "/api/v1/tickets?assignee=me", nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 1 {
		t.Fatalf("assignee=me for dana %v", got)
	}
	if got := f.do("GET", "/api/v1/tickets?assignee=me", nil, bearer(f.marco.Access)).expect(200).Items(); len(got) != 0 {
		t.Fatalf("assignee=me for marco %v", keys(got))
	}

	// Filters combine with every view: both must hold.
	list := func(who, q string) []string {
		return keys(f.do("GET", "/api/v1/tickets?"+q, nil, bearer(who)).expect(200).Items())
	}
	if got := list(f.dana.Access, "view=myWork&priority=1"); len(got) != 1 || got[0] != "1" {
		t.Fatalf("myWork+priority=1 %v", got)
	}
	if got := list(f.dana.Access, "view=myWork&priority=2"); len(got) != 0 {
		t.Fatalf("myWork+priority=2 %v", got)
	}
	f.do("GET", "/api/v1/tickets?category=resolved", nil, bearer(f.dana.Access)).expect(200) // the category filter is gone: ignored
	if got := list(f.marco.Access, "view=unassigned&tag=vip"); len(got) != 1 || got[0] != "2" {
		t.Fatalf("unassigned+tag %v", got)
	}
	if got := list(f.marco.Access, "view=unassigned&tag=other"); len(got) != 0 {
		t.Fatalf("unassigned+other tag %v", got)
	}
	if got := list(f.dana.Access, "view=resolved&priority=1"); len(got) != 0 {
		t.Fatalf("resolved+priority %v", got)
	}
	// The assignee is fixed by myWork and unassigned.
	f.do("GET", "/api/v1/tickets?view=myWork&assignee=me", nil, bearer(f.dana.Access)).expect(422)
	f.do("GET", "/api/v1/tickets?view=unassigned&assignee=me", nil, bearer(f.dana.Access)).expect(422)
}

func TestCannedReplies(t *testing.T) {
	f := newTicketFixture(t)
	f.createTicket(f.integration, map[string]any{"requester": map[string]any{"name": "Priya [x](https://evil.example)", "email": "priya@acme.example"}})
	mk := func(tok string, body map[string]any) result {
		return f.do("POST", "/api/v1/macros", body, bearer(tok))
	}

	// Name, body and audience only: actions are not part of the contract.
	mk(f.admin.Access, map[string]any{"name": "x", "body": "hi", "actions": []map[string]any{{"type": "setPriority", "priority": 2}}}).
		expect(422, "validationFailed")
	mk(f.admin.Access, map[string]any{"name": "x", "body": "Hi {{requester.email}}"}).expect(422)
	mk(f.admin.Access, map[string]any{"name": "x"}).expect(422)
	mk(f.admin.Access, map[string]any{"name": "x", "body": "   "}).expect(422)
	mk(f.admin.Access, map[string]any{"name": "x", "body": "hi", "visibility": "everyone"}).expect(422)
	mk(f.dana.Access, map[string]any{"name": "Mine", "body": "hello"}).expect(403)

	canned := mk(f.admin.Access, map[string]any{"name": "Escalated to Delivery",
		"body": "Hi {{requester.name}}, {{ticket.number}} is now with Delivery. — {{agent.name}}"}).expect(201).JSON()
	if _, has := canned["actions"]; has || canned["visibility"] != "public" || canned["active"] != true {
		t.Fatalf("canned reply %v", canned)
	}
	note := mk(f.admin.Access, map[string]any{"name": "Internal check", "body": "Checked by {{agent.name}}.", "visibility": "agent"}).expect(201).JSON()

	// Preview renders the body with escaped values for the composer and
	// changes nothing.
	before := f.version("1")
	msgsBefore, _ := f.coll("messages").CountDocuments(context.Background(), bson.D{})
	pv := f.do("GET", tpath("1", "/macros/"+canned["id"].(string)+"/preview"), nil, bearer(f.dana.Access)).expect(200).JSON()
	if pv["body"] != `Hi Priya \[x\]\(https://evil.example\), #1 is now with Delivery. — Dana Okafor` || pv["visibility"] != "public" ||
		pv["macroId"] != canned["id"] {
		t.Fatalf("preview %v", pv)
	}
	if pv := f.do("GET", tpath("1", "/macros/"+note["id"].(string)+"/preview"), nil, bearer(f.marco.Access)).expect(200).JSON(); pv["visibility"] != "agent" {
		t.Fatalf("note preview %v", pv)
	}
	if n, _ := f.coll("messages").CountDocuments(context.Background(), bson.D{}); n != msgsBefore || f.version("1") != before {
		t.Fatal("a preview changed the ticket or posted a message")
	}
	// The apply endpoint is gone.
	f.do("POST", tpath("1", "/macros/"+canned["id"].(string)+"/apply"), nil, bearer(f.dana.Access), ifMatchV(before)).expect(404)

	// Editing: administrators only, with If-Match.
	f.do("PATCH", "/api/v1/macros/"+canned["id"].(string), map[string]any{"name": "Renamed"}, bearer(f.dana.Access), ifMatchV(1)).expect(403)
	if r := f.do("PATCH", "/api/v1/macros/"+canned["id"].(string), map[string]any{"name": "Renamed"}, bearer(f.admin.Access), ifMatchV(1)).
		expect(200).JSON(); r["name"] != "Renamed" || r["version"] != float64(2) {
		t.Fatalf("patch %v", r)
	}
	// Integrations cannot see or use canned replies.
	f.do("GET", "/api/v1/macros", nil, bearer(f.integration)).expect(403, "userRequired")
}

func TestReportsSummary(t *testing.T) {
	f := newTicketFixture(t)
	f.createTicket(f.integration, map[string]any{})
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	f.reply(f.dana.Access, "1", "public", "Looking into it.")
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.reply(f.integration, "1", "public", "Still broken.") // requester reopens
	f.do("GET", "/api/v1/reports/summary", nil, bearer(f.dana.Access)).expect(403)
	f.do("GET", "/api/v1/reports/summary?from=2026-01-01&to=2026-06-01", nil, bearer(f.lead.Access)).expect(422)
	f.do("GET", "/api/v1/reports/summary?from=2026-02-01&to=2026-01-01", nil, bearer(f.lead.Access)).expect(422)
	r := f.do("GET", "/api/v1/reports/summary", nil, bearer(f.lead.Access)).expect(200).JSON()
	if r["kind"] != "boundedLiveTraversal" || !strings.Contains(obj(r, "bounds")["note"].(string), "not a stored point-in-time snapshot") {
		t.Fatalf("report kind %v", r)
	}
	backlog := obj(r, "backlog")
	if backlog["total"] != float64(2) {
		t.Fatalf("backlog %v", backlog)
	}
	days := r["createdVsResolved"].([]any)
	today := days[len(days)-1].(map[string]any)
	if len(days) != 30 || today["date"] != time.Now().UTC().Format("2006-01-02") || today["created"] != float64(2) || today["resolved"] != float64(1) {
		t.Fatalf("today %v (%d days)", today, len(days))
	}
	if fr := obj(r, "firstResponse"); fr["sampleSize"] != float64(1) || fr["medianSeconds"] == nil {
		t.Fatalf("first response %v", fr)
	}
	if ro := obj(r, "reopen"); ro["resolved"] != float64(1) || ro["reopened"] != float64(1) || ro["rate"] != float64(1) {
		t.Fatalf("reopen %v", ro)
	}
	if sb := obj(r, "slaBreaches"); sb["firstResponse"] != float64(0) || len(sb) != 1 {
		t.Fatalf("breaches %v", sb)
	}
}
