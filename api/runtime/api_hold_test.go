package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Owner decision 2026-10-31: On hold with managed hold reasons; no In
// progress status.

func (f *ticketFixture) holdReasonID(name string) string {
	f.t.Helper()
	for _, r := range f.do("GET", "/api/v1/hold-reasons", nil, bearer(f.admin.Access)).expect(200).Items() {
		if r["name"] == name {
			return r["id"].(string)
		}
	}
	f.t.Fatalf("no hold reason %q", name)
	return ""
}

func (f *ticketFixture) statusIDByCode(category string) string {
	f.t.Helper()
	for _, st := range f.do("GET", "/api/v1/statuses", nil, bearer(f.admin.Access)).expect(200).Items() {
		if st["category"] == category {
			return st["id"].(string)
		}
	}
	f.t.Fatalf("no status in category %s", category)
	return ""
}

func (f *ticketFixture) hold(tok, key, reasonID string, extra map[string]any) result {
	f.t.Helper()
	body := map[string]any{"action": "hold", "holdReasonId": reasonID}
	for k, v := range extra {
		body[k] = v
	}
	return f.do("POST", tpath(key, "/transitions"), body, bearer(tok), ifMatchV(f.version(key)))
}

func (f *ticketFixture) holdJobs() int64 {
	f.t.Helper()
	n, err := f.coll("jobs").CountDocuments(context.Background(), map[string]any{"kind": "ticket.holdExpire", "state": "queued"})
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestStatusesHaveNoInProgress(t *testing.T) {
	f := newTicketFixture(t)
	var names []string
	for _, st := range f.do("GET", "/api/v1/statuses", nil, bearer(f.dana.Access)).expect(200).Items() {
		names = append(names, st["name"].(string)+":"+st["category"].(string))
	}
	want := "Open:open,Awaiting customer:waitingCustomer,On hold:onHold,Resolved:resolved,Archived:archived"
	if strings.Join(names, ",") != want {
		t.Fatalf("statuses %v", names)
	}
	// Claiming keeps the ticket Open, with the assignee.
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	c := f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200).JSON()
	if obj(c, "status")["name"] != "Open" || c["assignee"] == nil {
		t.Fatalf("claim: %v", c)
	}
	// Releasing keeps it Open and unassigned.
	r := f.do("POST", tpath(key, "/release"), map[string]any{"reason": "going home"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200).JSON()
	if obj(r, "status")["name"] != "Open" || r["assignee"] != nil {
		t.Fatalf("release: %v", r)
	}
}

func TestHoldReasonsAdministration(t *testing.T) {
	f := newMCPFixture(t)
	_, reader := f.token("Reasons reader", "holdReasons:read")
	_, writer := f.token("Reasons writer", "holdReasons:write")
	_, stafft := f.token("Staff bot", "tickets:read", "tickets:write")
	_, all := f.token("All bot", "tickets:all")
	_, other := f.token("Create only", "tickets:create")

	// The six starting reasons are seeded by init, in name order.
	items := f.do("GET", "/api/v1/hold-reasons", nil, bearer(f.dana.Access)).expect(200).Items()
	if len(items) != 6 || items[0]["name"] != "Scheduled maintenance or change" || items[0]["maxDays"] != float64(14) || items[0]["inUse"] != float64(0) {
		t.Fatalf("seeded reasons %v", items)
	}
	days := map[string]float64{}
	for _, it := range items {
		days[it["name"].(string)] = it["maxDays"].(float64)
	}
	for name, d := range map[string]float64{"Waiting on Engineering": 5, "Waiting on vendor / third party": 5, "Waiting on another team": 3,
		"Scheduled maintenance or change": 14, "Waiting for deployment / release": 7, "Under investigation": 3} {
		if days[name] != d {
			t.Errorf("reason %q: %v days, want %v", name, days[name], d)
		}
	}

	// Scopes: read for every staff member, readers, writers and staff tokens;
	// tickets:all does not include the hold reason scopes (it reads them as a
	// staff token); a requester-side token cannot.
	for _, tok := range []string{reader, writer, stafft, all} {
		f.do("GET", "/api/v1/hold-reasons", nil, bearer(tok)).expect(200)
	}
	f.do("GET", "/api/v1/hold-reasons", nil, bearer(other)).expect(403, "insufficientScope")
	body := map[string]any{"name": "Waiting on legal", "maxDays": 10}
	for _, tok := range []string{f.dana.Access, reader, stafft, all, other} {
		f.do("POST", "/api/v1/hold-reasons", body, bearer(tok)).expect(403)
	}
	created := f.do("POST", "/api/v1/hold-reasons", body, bearer(writer)).expect(201).JSON()
	if created["name"] != "Waiting on legal" || created["maxDays"] != float64(10) || created["version"] != float64(1) {
		t.Fatalf("created %v", created)
	}
	id := created["id"].(string)
	f.do("POST", "/api/v1/hold-reasons", map[string]any{"name": "WAITING ON LEGAL", "maxDays": 2}, bearer(f.admin.Access)).expect(409, "holdReasonExists")
	for _, bad := range []map[string]any{{"name": "x", "maxDays": 0}, {"name": "x", "maxDays": 366}, {"name": "", "maxDays": 3}, {"name": "x"}} {
		f.do("POST", "/api/v1/hold-reasons", bad, bearer(f.admin.Access)).expect(422)
	}
	f.do("POST", "/api/v1/hold-reasons", map[string]any{"name": "x", "maxDays": 3, "active": true}, bearer(f.admin.Access)).expect(422)

	// Update and delete need If-Match; a changed maxDays is an update.
	f.do("PATCH", "/api/v1/hold-reasons/"+id, map[string]any{"maxDays": 12}, bearer(writer)).expect(428)
	f.do("PATCH", "/api/v1/hold-reasons/"+id, map[string]any{"maxDays": 12}, bearer(f.dana.Access), ifMatchV(1)).expect(403)
	up := f.do("PATCH", "/api/v1/hold-reasons/"+id, map[string]any{"maxDays": 12}, bearer(writer), ifMatchV(1)).expect(200).JSON()
	if up["maxDays"] != float64(12) || up["version"] != float64(2) {
		t.Fatalf("updated %v", up)
	}
	f.do("PATCH", "/api/v1/hold-reasons/"+id, map[string]any{"maxDays": 13}, bearer(writer), ifMatchV(1)).expect(409, "versionConflict")

	// Delete is refused while a ticket is on hold with the reason, and works
	// afterwards. Past holds keep the name on the ticket's activity.
	key := f.createTicket(f.ticketFixture.integration, map[string]any{}).Ref()
	h := f.hold(f.dana.Access, key, id, nil).expect(200).JSON()
	if obj(h, "hold")["reasonName"] != "Waiting on legal" {
		t.Fatalf("hold %v", h)
	}
	var inUse float64
	for _, it := range f.do("GET", "/api/v1/hold-reasons", nil, bearer(f.admin.Access)).expect(200).Items() {
		if it["id"] == id {
			inUse = it["inUse"].(float64)
		}
	}
	if inUse != 1 {
		t.Fatalf("inUse %v", inUse)
	}
	f.do("DELETE", "/api/v1/hold-reasons/"+id, nil, bearer(writer)).expect(428)
	f.do("DELETE", "/api/v1/hold-reasons/"+id, nil, bearer(writer), ifMatchV(2)).expect(409, "holdReasonInUse")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "setStatus", "statusId": f.waitingStatusID()},
		bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	f.do("DELETE", "/api/v1/hold-reasons/"+id, nil, bearer(f.dana.Access), ifMatchV(2)).expect(403)
	f.do("DELETE", "/api/v1/hold-reasons/"+id, nil, bearer(writer), ifMatchV(2)).expect(204)
	f.do("DELETE", "/api/v1/hold-reasons/"+id, nil, bearer(writer), ifMatchV(2)).expect(404, "holdReasonNotFound")
	if !strings.Contains(string(f.do("GET", tpath(key, "/activity?limit=100"), nil, bearer(f.admin.Access)).expect(200).Body), "Waiting on legal") {
		t.Fatal("the past hold lost its reason name")
	}
	// Holding with a deleted reason fails.
	f.hold(f.dana.Access, key, id, nil).expect(422)

	// Every change was audited (tokens with the scope).
	evs, err := f.c.Audit.RecentAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.Action] = true
	}
	for _, a := range []string{"holdReason.created", "holdReason.updated", "holdReason.deleted", "ticket.held"} {
		if !seen[a] {
			t.Errorf("%s not audited", a)
		}
	}

	// MCP parity.
	s := f.connect(writer)
	got := mustCall(t, s, "holdReasonsCreate", map[string]any{"name": "Waiting on payment", "maxDays": 6})
	mid := got["id"].(string)
	if got["maxDays"] != float64(6) {
		t.Fatalf("holdReasonsCreate %v", got)
	}
	mustFail(t, s, "holdReasonsCreate", map[string]any{"name": "Waiting on payment", "maxDays": 6}, "holdReasonExists")
	if got := mustCall(t, s, "holdReasonsUpdate", map[string]any{"holdReasonId": mid, "maxDays": 9}); got["maxDays"] != float64(9) {
		t.Fatalf("holdReasonsUpdate %v", got)
	}
	if got := mustCall(t, f.connect(reader), "holdReasonsList", nil); len(got["items"].([]any)) != 7 {
		t.Fatalf("holdReasonsList %v", got)
	}
	mustFail(t, f.connect(reader), "holdReasonsCreate", map[string]any{"name": "x", "maxDays": 1}, "insufficientScope")
	mustFail(t, f.connect(stafft), "holdReasonsDelete", map[string]any{"holdReasonId": mid}, "insufficientScope")
	if got := mustCall(t, s, "holdReasonsDelete", map[string]any{"holdReasonId": mid}); got["deleted"] != mid {
		t.Fatalf("holdReasonsDelete %v", got)
	}
}

func TestHoldRulesAndExpiry(t *testing.T) {
	f := newAutomationFixture(t)
	eng := f.holdReasonID("Waiting on Engineering") // 5 days
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	f.runDue()
	f.mail.Reset()

	// A reason is required, and only Open or Awaiting customer tickets can be
	// put on hold; On hold is not selectable with setStatus.
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "hold"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(422)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "hold", "holdReasonId": "bad"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(422)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve", "holdReasonId": eng}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(422)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "setStatus", "statusId": f.statusIDByCode("onHold")},
		bearer(f.dana.Access), ifMatchV(f.version(key))).expect(422)
	other := f.createTicket(f.integration, map[string]any{"subject": "Other"}).Ref()
	f.do("POST", tpath(other, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(other))).expect(200)
	f.hold(f.dana.Access, other, eng, nil).expect(409, "invalidTransition")
	f.runDue()
	f.mail.Reset()

	// Hold with a note: the length is exactly the reason's maxDays.
	start := f.clk.Now()
	h := f.hold(f.dana.Access, key, eng, map[string]any{"note": "Engineering is checking the MX records."}).expect(200).JSON()
	hold := obj(h, "hold")
	until, err := time.Parse(time.RFC3339, hold["until"].(string))
	if err != nil || !until.Equal(start.Add(5*24*time.Hour)) || hold["reasonName"] != "Waiting on Engineering" || hold["reasonId"] != eng {
		t.Fatalf("hold %v (start %v)", hold, start)
	}
	if obj(h, "status")["category"] != "onHold" || obj(h, "status")["name"] != "On hold" || obj(h, "assignee")["name"] != "Dana Okafor" {
		t.Fatalf("held ticket %v", h)
	}
	msgs := f.do("GET", tpath(key, "/messages"), nil, bearer(f.dana.Access)).expect(200).Items()
	if last := msgs[len(msgs)-1]; last["visibility"] != "agent" || last["body"] != "Engineering is checking the MX records." {
		t.Fatalf("note %v", last)
	}
	// The requester side sees the status but no hold details and no email.
	rv := f.get(f.integration, key).expect(200).JSON()
	if rv["hold"] != nil || obj(rv, "status")["name"] != "On hold" {
		t.Fatalf("requester view %v", rv)
	}
	f.runDue()
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 0 {
		t.Fatalf("requester notified about the hold: %+v", got)
	}
	// The Awaiting customer and On hold queues.
	if got := keys(f.do("GET", "/api/v1/tickets?view=onHold", nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 1 || got[0] != key {
		t.Fatalf("onHold queue %v", got)
	}
	if got := keys(f.do("GET", "/api/v1/tickets?view=myWork", nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 0 {
		t.Fatalf("myWork shows a held ticket: %v", got)
	}
	counts := f.do("GET", "/api/v1/queue-counts", nil, bearer(f.dana.Access)).expect(200).JSON()
	if counts["onHold"] != float64(1) || counts["awaitingCustomer"] != float64(0) {
		t.Fatalf("counts %v", counts)
	}
	if f.holdJobs() != 1 {
		t.Fatalf("hold jobs: %d", f.holdJobs())
	}

	// A longer maxDays change affects only new holds.
	f.do("PATCH", "/api/v1/hold-reasons/"+eng, map[string]any{"maxDays": 30}, bearer(f.admin.Access), ifMatchV(1)).expect(200)
	if u := obj(f.get(f.dana.Access, key).expect(200).JSON(), "hold")["until"]; u != hold["until"] {
		t.Fatalf("existing hold changed: %v", u)
	}

	// Still on hold just before the end; back to Open after it.
	f.advance(5*24*time.Hour - time.Minute)
	if f.category(key) != "onHold" {
		t.Fatal("hold ended early")
	}
	f.advance(2 * time.Minute)
	back := f.get(f.admin.Access, key).expect(200).JSON()
	if obj(back, "status")["category"] != "open" || back["hold"] != nil || obj(back, "assignee")["name"] != "Dana Okafor" {
		t.Fatalf("after hold %v", back)
	}
	sent := sentTo(f.mail, "dana@example.com")
	if len(sent) != 1 || sent[0].Subject != "[#"+key+"] Back from hold: Waiting on Engineering" || !strings.Contains(sent[0].Text, "Open again") {
		t.Fatalf("assignee emails: %+v", sent)
	}
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 0 {
		t.Fatalf("requester notified about the end of the hold: %+v", got)
	}
	var ended bool
	for _, it := range f.do("GET", tpath(key, "/activity?limit=100"), nil, bearer(f.admin.Access)).expect(200).Items() {
		if it["action"] == "ticket.holdEnded" {
			ended = obj(it, "actor")["type"] == "system" && strings.Contains(it["reason"].(string), "Waiting on Engineering")
		}
	}
	if !ended {
		t.Fatal("hold end not recorded as automation activity")
	}
	if f.holdJobs() != 0 {
		t.Fatalf("hold job left: %d", f.holdJobs())
	}

	// The first-response SLA keeps running while on hold: nothing paused it.
	// An unassigned held ticket reappears in the queue without any email.
	un := f.createTicket(f.integration, map[string]any{"subject": "Unassigned"}).Ref()
	f.hold(f.dana.Access, un, f.holdReasonID("Waiting on another team"), nil).expect(200) // 3 days
	f.runDue()
	f.mail.Reset()
	f.advance(4 * 24 * time.Hour)
	if f.category(un) != "open" {
		t.Fatalf("unassigned hold: %s", f.category(un))
	}
	if got := sentTo(f.mail, "dana@example.com"); len(got) != 0 {
		t.Fatalf("emails for an unassigned ticket: %+v", got)
	}
	if got := keys(f.do("GET", "/api/v1/tickets?view=unassigned", nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 1 || got[0] != un {
		t.Fatalf("unassigned queue %v", got)
	}
}

func TestHoldEndsWithReplyOrStatusChange(t *testing.T) {
	f := newAutomationFixture(t)
	eng := f.holdReasonID("Waiting on Engineering")
	setup := func(subject string) string {
		key := f.createTicket(f.integration, map[string]any{"subject": subject}).Ref()
		f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
		f.hold(f.dana.Access, key, eng, nil).expect(200)
		return key
	}

	// A requester reply ends the hold: Open, assignee kept, assignee emailed
	// like any requester reply, job cancelled.
	a := setup("Reply ends hold")
	f.runDue()
	f.mail.Reset()
	if f.holdJobs() != 1 {
		t.Fatalf("jobs %d", f.holdJobs())
	}
	f.do("POST", tpath(a, "/messages"), map[string]any{"visibility": "public", "body": "Any news?"}, bearer(f.integration)).expect(201)
	f.runDue()
	got := f.get(f.admin.Access, a).expect(200).JSON()
	if obj(got, "status")["category"] != "open" || got["hold"] != nil || obj(got, "assignee")["name"] != "Dana Okafor" {
		t.Fatalf("after reply %v", got)
	}
	if f.holdJobs() != 0 {
		t.Fatalf("job not cancelled: %d", f.holdJobs())
	}
	if len(sentTo(f.mail, "dana@example.com")) == 0 {
		t.Fatal("assignee not notified of the requester reply")
	}
	f.mail.Reset()
	f.advance(6 * 24 * time.Hour)
	if len(sentTo(f.mail, "dana@example.com")) != 0 {
		t.Fatal("expired job still emailed")
	}

	// A staff status change ends the hold and cancels the job.
	b := setup("Staff ends hold")
	f.do("POST", tpath(b, "/transitions"), map[string]any{"action": "setStatus", "statusId": f.waitingStatusID()},
		bearer(f.dana.Access), ifMatchV(f.version(b))).expect(200)
	got = f.get(f.admin.Access, b).expect(200).JSON()
	if obj(got, "status")["category"] != "waitingCustomer" || got["hold"] != nil || f.holdJobs() != 0 {
		t.Fatalf("after status change %v (jobs %d)", got, f.holdJobs())
	}
	// Resolving a held ticket also ends the hold.
	c := setup("Resolve ends hold")
	f.do("POST", tpath(c, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(c))).expect(200)
	if got = f.get(f.admin.Access, c).expect(200).JSON(); got["hold"] != nil || f.holdJobs() != 0 {
		t.Fatalf("after resolve %v", got)
	}

	// Awaiting customer tickets can be put on hold; the requester's reply
	// then reopens it.
	d := f.createTicket(f.integration, map[string]any{"subject": "From awaiting"}).Ref()
	f.waiting(f.dana.Access, d)
	f.hold(f.dana.Access, d, eng, nil).expect(200)
	if f.category(d) != "onHold" {
		t.Fatalf("hold from awaiting customer: %s", f.category(d))
	}
}

func TestHoldWithTokens(t *testing.T) {
	f := newMCPFixture(t)
	eng := f.holdReasonID("Waiting on Engineering")
	_, writer := f.token("Hold bot", "tickets:read", "tickets:write")
	_, noter := f.token("Note bot", "tickets:read", "tickets:write", "tickets:internal")
	_, reader := f.token("Read bot", "tickets:read")
	key := f.createTicket(f.ticketFixture.integration, map[string]any{}).Ref()

	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "hold", "holdReasonId": eng}, bearer(reader), ifMatchV(f.version(key))).expect(403)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "hold"}, bearer(writer), ifMatchV(f.version(key))).expect(422)
	// A note needs the internal-note permission.
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "hold", "holdReasonId": eng, "note": "n"}, bearer(writer), ifMatchV(f.version(key))).expect(403)
	got := f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "hold", "holdReasonId": eng}, bearer(writer), ifMatchV(f.version(key))).expect(200).JSON()
	if obj(got, "status")["category"] != "onHold" || obj(got, "hold")["reasonId"] != eng {
		t.Fatalf("token hold %v", got)
	}

	// MCP: the transition tool takes the reason; it is required there too.
	k2 := f.createTicket(f.ticketFixture.integration, map[string]any{"subject": "Via MCP"}).Ref()
	s := f.connect(noter)
	mustFail(t, s, "ticketsTransition", map[string]any{"ticket": k2, "action": "hold"}, "validationFailed")
	out := mustCall(t, s, "ticketsTransition", map[string]any{"ticket": k2, "action": "hold", "holdReasonId": eng, "note": "Waiting for the vendor"})
	if obj(out, "status")["category"] != "onHold" || obj(out, "hold")["reasonName"] != "Waiting on Engineering" {
		t.Fatalf("mcp hold %v", out)
	}
}

// A lost expiry job must not leave a ticket on hold forever: the periodic
// sweep ends holds that ran out, through the same path as the job.
func TestHoldSweepEndsHoldWithoutJob(t *testing.T) {
	f := newAutomationFixture(t)
	eng := f.holdReasonID("Under investigation") // 3 days
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	f.hold(f.dana.Access, key, eng, nil).expect(200)
	if _, err := f.coll("jobs").DeleteMany(context.Background(), map[string]any{"kind": "ticket.holdExpire"}); err != nil {
		t.Fatal(err)
	}
	f.runDue()
	f.mail.Reset()
	f.clk.Advance(3*24*time.Hour + 5*time.Minute)
	if n, err := f.c.Tickets.SweepExpiredHolds(context.Background(), 10); err != nil || n != 0 {
		t.Fatalf("sweep inside the grace period: %d %v", n, err)
	}
	f.clk.Advance(10 * time.Minute)
	if n, err := f.c.Tickets.SweepExpiredHolds(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	got := f.get(f.admin.Access, key).expect(200).JSON()
	if obj(got, "status")["category"] != "open" || got["hold"] != nil || obj(got, "assignee")["name"] != "Dana Okafor" {
		t.Fatalf("after sweep %v", got)
	}
	f.runDue()
	if sent := sentTo(f.mail, "dana@example.com"); len(sent) != 1 || !strings.Contains(sent[0].Subject, "Back from hold") {
		t.Fatalf("assignee emails: %+v", sent)
	}
	if n, _ := f.c.Tickets.SweepExpiredHolds(context.Background(), 10); n != 0 {
		t.Fatalf("second sweep ended %d", n)
	}
}

// An assignee who left the group while the ticket was on hold is not kept
// when the hold ends.
func TestHoldEndClearsIneligibleAssignee(t *testing.T) {
	f := newAutomationFixture(t)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	f.hold(f.dana.Access, key, f.holdReasonID("Under investigation"), nil).expect(200)
	f.do("DELETE", "/api/v1/groups/"+f.triage+"/members/"+f.dana.UserId, nil, bearer(f.admin.Access)).expect(204)
	f.runDue()
	f.advance(4 * 24 * time.Hour)
	got := f.get(f.admin.Access, key).expect(200).JSON()
	if obj(got, "status")["category"] != "open" || got["assignee"] != nil {
		t.Fatalf("after hold %v", got)
	}
}
