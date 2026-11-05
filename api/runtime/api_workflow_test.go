package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/clock"
)

func linkRelations(r result) map[string]string {
	out := map[string]string{}
	for _, it := range r.Items() {
		out[refOf(obj(it, "ticket"))] = it["relation"].(string)
	}
	return out
}

func TestTicketLinksAuthorizeEachSide(t *testing.T) {
	f := newTicketFixture(t)
	a := f.createTicket(f.dana.Access, map[string]any{"subject": "Mailbox full"}).Ref()                          // Triage
	b := f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery, "subject": "Delivery only"}).Ref() // Dana cannot see
	c := f.createTicket(f.dana.Access, map[string]any{"subject": "Quota warning"}).Ref()

	// Validation and visibility of the other side when linking.
	f.do("POST", tpath(a, "/links"), map[string]any{"type": "blocks", "ticket": c}, bearer(f.lead.Access)).expect(422)
	f.do("POST", tpath(a, "/links"), map[string]any{"type": "relatedTo", "ticket": a}, bearer(f.lead.Access)).expect(422)
	f.do("POST", tpath(a, "/links"), map[string]any{"type": "relatedTo", "ticket": b}, bearer(f.dana.Access)).expect(422) // b is invisible to Dana
	f.do("POST", tpath(a, "/links"), map[string]any{"type": "relatedTo", "ticket": c}, bearer(f.integration)).expect(403, "userRequired")

	ab := f.do("POST", tpath(a, "/links"), map[string]any{"type": "duplicateOf", "ticket": b}, bearer(f.lead.Access)).expect(201).JSON()
	if ab["relation"] != "duplicateOf" || refOf(obj(ab, "ticket")) != b {
		t.Fatalf("link: %v", ab)
	}
	f.do("POST", tpath(a, "/links"), map[string]any{"type": "duplicateOf", "ticket": b}, bearer(f.lead.Access)).expect(409, "linkExists")
	f.do("POST", tpath(c, "/links"), map[string]any{"type": "relatedTo", "ticket": a}, bearer(f.dana.Access)).expect(201)
	// relatedTo is symmetric: the reverse direction is the same link.
	f.do("POST", tpath(a, "/links"), map[string]any{"type": "relatedTo", "ticket": c}, bearer(f.dana.Access)).expect(409, "linkExists")

	// The lead sees both links, each from the viewed ticket's perspective.
	if got := linkRelations(f.do("GET", tpath(a, "/links"), nil, bearer(f.lead.Access)).expect(200)); got[b] != "duplicateOf" || got[c] != "relatedTo" || len(got) != 2 {
		t.Fatalf("lead links of A: %v", got)
	}
	if got := linkRelations(f.do("GET", tpath(b, "/links"), nil, bearer(f.lead.Access)).expect(200)); got[a] != "duplicatedBy" {
		t.Fatalf("lead links of B: %v", got)
	}
	// Dana cannot read B: the link to it is omitted entirely, and she cannot
	// remove it.
	danaLinks := f.do("GET", tpath(a, "/links"), nil, bearer(f.dana.Access)).expect(200)
	if got := linkRelations(danaLinks); len(got) != 1 || got[c] != "relatedTo" || strings.Contains(string(danaLinks.Body), `"number":`+b+`,`) {
		t.Fatalf("Dana's links of A leak the hidden side: %s", danaLinks.Body)
	}
	f.do("GET", tpath(b, "/links"), nil, bearer(f.dana.Access)).expect(404, "ticketNotFound")
	f.do("DELETE", tpath(a, "/links/", ab["id"].(string)), nil, bearer(f.dana.Access)).expect(404, "linkNotFound")
	// Integrations (all project tickets) read links but do not change them.
	if got := linkRelations(f.do("GET", tpath(a, "/links"), nil, bearer(f.integration)).expect(200)); len(got) != 2 {
		t.Fatalf("integration links: %v", got)
	}
	f.do("DELETE", tpath(a, "/links/", ab["id"].(string)), nil, bearer(f.lead.Access)).expect(204)
	f.do("DELETE", tpath(a, "/links/", ab["id"].(string)), nil, bearer(f.lead.Access)).expect(404, "linkNotFound")
	if got := linkRelations(f.do("GET", tpath(b, "/links"), nil, bearer(f.lead.Access)).expect(200)); len(got) != 0 {
		t.Fatalf("links after removal: %v", got)
	}
}

func TestFollowUpFromArchivedTicket(t *testing.T) {
	f := newTicketFixture(t)
	orig := f.createTicket(f.integration, map[string]any{"subject": "Bounced invoices", "priority": 2}).JSON()
	key := refOf(orig)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "SECRET-NOTE do not copy"}, bearer(f.dana.Access)).expect(201)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	archived := f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200).JSON()
	archivedVersion := int64(archived["version"].(float64))

	// Replies to the archived ticket are rejected with the follow-up action.
	for _, tok := range []string{f.dana.Access, f.integration} {
		p := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Still broken"}, bearer(tok)).expect(409, "ticketArchived")
		var prob struct {
			Actions []struct{ Rel, Method, Href, Title string } `json:"actions"`
		}
		json.Unmarshal(p.Body, &prob)
		if len(prob.Actions) != 1 || prob.Actions[0].Rel != "createFollowUp" || prob.Actions[0].Method != "POST" ||
			prob.Actions[0].Href != "/api/v1/tickets/"+key+"/follow-up" || prob.Actions[0].Title != "Create follow-up ticket" {
			t.Fatalf("archived problem actions: %s", p.Body)
		}
	}
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "reopen"}, bearer(f.dana.Access), ifMatchV(archivedVersion)).expect(409, "ticketArchived")

	// The action creates a linked ticket copying only subject, requester,
	// request type and group.
	fu := f.do("POST", tpath(key, "/follow-up"), map[string]any{}, bearer(f.dana.Access), header("Idempotency-Key", "fu-1")).expect(201)
	nt := fu.JSON()
	nk := refOf(nt)
	if nk == key || nt["subject"] != "Bounced invoices" || obj(nt, "requester")["id"] != obj(orig, "requester")["id"] ||
		obj(nt, "group")["id"] != f.triage || obj(nt, "requestType")["id"] != obj(orig, "requestType")["id"] ||
		obj(nt, "status")["category"] != "open" || nt["assignee"] != nil || nt["priority"] != float64(3) || fu.Header.Get("Location") == "" {
		t.Fatalf("follow-up: %v", nt)
	}
	// Idempotent replay returns the same ticket.
	if again := f.do("POST", tpath(key, "/follow-up"), map[string]any{}, bearer(f.dana.Access), header("Idempotency-Key", "fu-1")).expect(201).Ref(); again != nk {
		t.Fatalf("replay created %s, want %s", again, nk)
	}
	msgs := f.do("GET", tpath(nk, "/messages"), nil, bearer(f.dana.Access)).expect(200)
	if items := msgs.Items(); len(items) != 1 || items[0]["visibility"] != "public" || items[0]["body"] != "Follow-up to #"+key+"." ||
		strings.Contains(string(msgs.Body), "SECRET-NOTE") {
		t.Fatalf("follow-up messages: %s", msgs.Body)
	}
	if got := linkRelations(f.do("GET", tpath(nk, "/links"), nil, bearer(f.dana.Access)).expect(200)); got[key] != "followUpTo" {
		t.Fatalf("follow-up link: %v", got)
	}
	if got := linkRelations(f.do("GET", tpath(key, "/links"), nil, bearer(f.dana.Access)).expect(200)); got[nk] != "followedUpBy" {
		t.Fatalf("original link: %v", got)
	}
	// The original stays archived and unchanged.
	o := f.get(f.dana.Access, key).expect(200).JSON()
	if obj(o, "status")["category"] != "archived" || int64(o["version"].(float64)) != archivedVersion {
		t.Fatalf("original changed: %v", o)
	}
	// Integrations use the same action (authored as the requester).
	it := f.do("POST", tpath(key, "/follow-up"), map[string]any{"body": "It happened again."}, bearer(f.integration)).expect(201).JSON()
	if it["source"] != "api" || obj(it, "requester")["id"] != obj(orig, "requester")["id"] {
		t.Fatalf("integration follow-up: %v", it)
	}
	f.do("POST", tpath(key, "/follow-up"), map[string]any{}, bearer(f.readOnlyIntegraton)).expect(403)
	f.do("POST", tpath(key, "/follow-up"), map[string]any{"subject": ""}, bearer(f.dana.Access)).expect(422)
}

func TestActivityTimelineHasNoBodies(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{"body": "PUBLIC-BODY-TEXT"}).Ref()
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "SECRET-NOTE-BODY"}, bearer(f.dana.Access)).expect(201)
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve", "reason": "Mailbox emptied"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)

	r := f.do("GET", tpath(key, "/activity"), nil, bearer(f.dana.Access)).expect(200)
	var actions []string
	for _, it := range r.Items() {
		actions = append(actions, it["action"].(string))
		if obj(it, "actor")["type"] == "user" && obj(it, "actor")["name"] != "Dana Okafor" {
			t.Fatalf("actor name: %v", it)
		}
	}
	if !slices.Equal(actions, []string{"ticket.created", "message.created", "ticket.claimed", "ticket.resolved"}) {
		t.Fatalf("actions: %v", actions)
	}
	if body := string(r.Body); strings.Contains(body, "PUBLIC-BODY-TEXT") || strings.Contains(body, "SECRET-NOTE-BODY") ||
		!strings.Contains(body, "Mailbox emptied") || !strings.Contains(body, "visibility:agent") {
		t.Fatalf("activity projection: %s", body)
	}
	// Paging is complete and stable.
	var paged []string
	cursor := ""
	for i := 0; i < 10; i++ {
		p := f.do("GET", tpath(key, "/activity?limit=1"+cursor), nil, bearer(f.dana.Access)).expect(200)
		for _, it := range p.Items() {
			paged = append(paged, it["id"].(string))
		}
		next, _ := p.JSON()["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = "&cursor=" + next
	}
	if len(paged) != 4 {
		t.Fatalf("paged %d entries", len(paged))
	}
	// order=desc is the same list newest first, and pages stay complete.
	var newest []string
	cursor = ""
	for i := 0; i < 10; i++ {
		p := f.do("GET", tpath(key, "/activity?order=desc&limit=3"+cursor), nil, bearer(f.dana.Access)).expect(200)
		for _, it := range p.Items() {
			newest = append(newest, it["id"].(string))
		}
		next, _ := p.JSON()["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = "&cursor=" + next
	}
	slices.Reverse(newest)
	if !slices.Equal(newest, paged) {
		t.Fatalf("order=desc %v, want the reverse of %v", newest, paged)
	}
	// One exact action only.
	if one := f.do("GET", tpath(key, "/activity?action=ticket.claimed"), nil, bearer(f.dana.Access)).expect(200).Items(); len(one) != 1 || one[0]["action"] != "ticket.claimed" {
		t.Fatalf("action filter: %v", one)
	}
	f.do("GET", tpath(key, "/activity?action=user.created"), nil, bearer(f.dana.Access)).expect(422)
	f.do("GET", tpath(key, "/activity?order=sideways"), nil, bearer(f.dana.Access)).expect(422)
	// Staff only; other groups' tickets stay hidden.
	f.do("GET", tpath(key, "/activity"), nil, bearer(f.integration)).expect(403, "userRequired")
	other := f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery}).Ref()
	f.do("GET", tpath(other, "/activity"), nil, bearer(f.dana.Access)).expect(404, "ticketNotFound")
}

func TestRedactionRemovesBodyFromReadsAndSearch(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.integration, map[string]any{"subject": "Login trouble"}).Ref()
	msg := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "my password is pineapple42 sorry"},
		bearer(f.integration)).expect(201).JSON()
	mid := msg["id"].(string)
	f.do("PUT", tpath(key, "/drafts/public"), map[string]any{"body": "draft mentioning pineapple42"}, bearer(f.dana.Access)).expect(200)
	if f.search(f.integration, "pineapple42").JSON()["total"] != float64(1) || f.search(f.admin.Access, "pineapple42").JSON()["total"] != float64(1) {
		t.Fatal("message not searchable before redaction")
	}
	before := f.version(key)

	f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "Customer posted a password"}, bearer(f.dana.Access)).expect(403)
	f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "x"}, bearer(f.admin.Access)).expect(422)
	f.do("POST", tpath(key, "/messages/000000000000000000000000/redact"), map[string]any{"reason": "Customer posted a password"}, bearer(f.admin.Access)).expect(404, "messageNotFound")
	red := f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "Customer posted a password"}, bearer(f.admin.Access)).expect(200).JSON()
	if red["body"] != "[redacted]" || red["redactedAt"] == nil {
		t.Fatalf("redacted: %v", red)
	}
	f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "Customer posted a password"}, bearer(f.admin.Access)).expect(409, "messageAlreadyRedacted")
	if f.version(key) != before+1 {
		t.Fatal("redaction must move the ticket version")
	}
	// Message reads (staff and integration) and search no longer contain it.
	for _, tok := range []string{f.dana.Access, f.integration} {
		body := string(f.do("GET", tpath(key, "/messages"), nil, bearer(tok)).expect(200).Body)
		if strings.Contains(body, "pineapple42") || !strings.Contains(body, "[redacted]") {
			t.Fatalf("messages after redaction: %s", body)
		}
		if s := f.search(tok, "pineapple42").JSON(); s["total"] != float64(0) {
			t.Fatalf("search after redaction: %v", s)
		}
	}
	// Drafts are unaffected; the audit keeps actor and reason, not the body.
	if d := f.do("GET", tpath(key, "/drafts/public"), nil, bearer(f.dana.Access)).expect(200).JSON(); d["body"] != "draft mentioning pineapple42" {
		t.Fatalf("draft changed: %v", d)
	}
	act := f.do("GET", tpath(key, "/activity"), nil, bearer(f.dana.Access)).expect(200)
	if !strings.Contains(string(act.Body), "message.redacted") || !strings.Contains(string(act.Body), "Customer posted a password") ||
		strings.Contains(string(act.Body), "pineapple42") {
		t.Fatalf("redaction audit: %s", act.Body)
	}
	n, err := f.coll("auditEvents").CountDocuments(context.Background(), map[string]any{"changes": map[string]any{"$regex": "pineapple"}})
	if err != nil || n != 0 {
		t.Fatalf("audit holds the body: %d %v", n, err)
	}
}

// ---- Automation ----

type automationFixture struct {
	*notifyFixture
	clk *clock.Manual
}

func newAutomationFixture(t *testing.T) *automationFixture {
	t.Helper()
	f := &automationFixture{notifyFixture: newNotifyFixture(t), clk: clock.NewManual(slaStart)}
	f.c.UseBusinessClock(f.clk)
	f.h = f.router(f.c)
	return f
}

func (f *automationFixture) waiting(tok, key string) {
	f.t.Helper()
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "setStatus", "statusId": f.waitingStatusID()},
		bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
}

func (f *automationFixture) advance(d time.Duration) {
	f.t.Helper()
	f.clk.Advance(d)
	f.runDue()
}

func (f *automationFixture) reminders(key string) int {
	f.t.Helper()
	n := 0
	for _, it := range f.do("GET", tpath(key, "/activity?limit=100"), nil, bearer(f.admin.Access)).expect(200).Items() {
		if it["action"] == "ticket.followupReminderSent" {
			n++
		}
	}
	return n
}

func (f *automationFixture) category(key string) string {
	f.t.Helper()
	return obj(f.get(f.admin.Access, key).expect(200).JSON(), "status")["category"].(string)
}

func TestAutomationRemindersAutoResolveAndArchive(t *testing.T) {
	f := newAutomationFixture(t)
	path := "/api/v1/automation"
	p := f.do("GET", path, nil, bearer(f.dana.Access)).expect(200).JSON()
	if p["enabled"] != false || p["version"] != float64(0) {
		t.Fatalf("default policy: %v", p)
	}
	policy := map[string]any{"archiveAfterResolvedDays": 3, "customerFollowupAfterHours": 24, "maxReminders": 2,
		"reminderCooldownHours": 24, "autoResolveAfterHours": 48}
	f.do("PUT", path, policy, bearer(f.dana.Access), header("If-Match", "0")).expect(403)
	f.do("PUT", path, map[string]any{"customerFollowupAfterHours": 0, "maxReminders": 2, "reminderCooldownHours": 24,
		"autoResolveAfterHours": 48}, bearer(f.admin.Access), header("If-Match", "0")).expect(422)
	f.do("PUT", path, policy, bearer(f.admin.Access)).expect(428)

	// Tickets: waiting (T1), resolved (T2), waiting then answered by the
	// requester (T4).
	t1 := f.createTicket(f.integration, map[string]any{"subject": "Waiting one"}).Ref()
	t2 := f.createTicket(f.integration, map[string]any{"subject": "Resolved one"}).Ref()
	t4 := f.createTicket(f.integration, map[string]any{"subject": "Answered one"}).Ref()
	for _, k := range []string{t1, t4} {
		f.waiting(f.dana.Access, k)
	}
	f.do("POST", tpath(t2, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(t2))).expect(200)
	f.drain()
	f.mail.Reset()

	saved := f.do("PUT", path, policy, bearer(f.admin.Access), header("If-Match", "0")).expect(200).JSON()
	if saved["enabled"] != true || saved["version"] != float64(1) {
		t.Fatalf("saved: %v", saved)
	}
	f.do("PUT", path, policy, bearer(f.admin.Access), header("If-Match", "0")).expect(409, "versionConflict")
	f.runDue()
	if f.reminders(t1) != 0 {
		t.Fatal("reminder before the follow-up time")
	}

	// 25 h: first reminders for T1 and T4 (email to the requester only).
	f.advance(25 * time.Hour)
	f.runDue()
	if f.reminders(t1) != 1 || f.reminders(t4) != 1 {
		t.Fatalf("first reminders: %d %d", f.reminders(t1), f.reminders(t4))
	}
	n, _ := f.coll("auditEvents").CountDocuments(context.Background(), map[string]any{"action": "ticket.followupReminderSent"})
	if n != 2 {
		t.Fatalf("reminders recorded: %d", n)
	}
	sent := sentTo(f.mail, "priya@acme.example")
	if len(sent) != 2 || !strings.Contains(sent[0].Subject, "waiting for your reply") || strings.Contains(sent[0].Text, "marked as resolved") ||
		!strings.Contains(sent[0].Text, "open the system you used to submit it") || strings.Contains(sent[0].Text, "https://") {
		t.Fatalf("reminder emails: %d %+v", len(sent), sent)
	}
	for _, d := range f.deliveriesFor("ticket.followupReminder") {
		if d.Recipient.Kind != "contact" || d.Template != "followupReminder" {
			t.Fatalf("reminder delivery: %+v", d)
		}
	}

	// Cooldown: no second reminder an hour later.
	f.advance(time.Hour)
	if f.reminders(t1) != 1 {
		t.Fatal("reminder cooldown ignored")
	}

	// The requester answers T4: its timers are cancelled in the reply
	// transaction. A job for its old generation is rejected.
	var gen4 string
	{
		var doc struct {
			Automation struct {
				Gen int64 `bson:"gen"`
			} `bson:"automation"`
		}
		n4, _ := strconv.ParseInt(t4, 10, 64)
		f.coll("tickets").FindOne(context.Background(), map[string]any{"number": n4}).Decode(&doc)
		gen4 = strconv.FormatInt(doc.Automation.Gen, 10)
	}
	f.do("POST", tpath(t4, "/messages"), map[string]any{"visibility": "public", "body": "Here is the information."}, bearer(f.integration)).expect(201)
	if f.category(t4) == "waitingCustomer" {
		t.Fatal("requester reply did not leave waiting")
	}
	f.waiting(f.dana.Access, t4) // waiting again: a fresh clock
	id4 := f.get(f.admin.Access, t4).expect(200).Str("id")
	if _, err := f.c.Jobs.Enqueue(context.Background(), jobs.NewJob{Kind: automation.KindAct, Key: "test-stale:" + id4,
		WorkspaceId: f.c.Settings.Current().WorkspaceID(), ProjectId: f.projectID(),
		Payload: map[string]string{"projectId": f.projectID(), "ticketId": id4, "generation": gen4, "action": "remind", "reminders": "0"},
		RunAt:   f.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	// The repository guard itself: a reminder for the old generation is
	// rejected even when the reminder count matches.
	cur, err := f.c.Tickets.AutomationTicket(context.Background(), f.projectID(), id4)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := strconv.ParseInt(gen4, 10, 64)
	if cur.Automation.Generation == old || cur.Automation.Reminders != 0 {
		t.Fatalf("reply did not move the automation generation: %+v", cur.Automation)
	}
	if err := f.c.Tickets.RemindRequester(context.Background(), cur, old, 0, false); !errors.Is(err, tickets.ErrStale) {
		t.Fatalf("stale generation accepted: %v", err)
	}

	// +23 h (49 h): T1's second reminder carries the auto-resolve notice; T4's
	// stale job did nothing and its new clock has not run out yet.
	f.advance(23 * time.Hour)
	f.runDue()
	if f.reminders(t1) != 2 || f.reminders(t4) != 1 {
		t.Fatalf("second reminders: %d %d", f.reminders(t1), f.reminders(t4))
	}
	if last := sentTo(f.mail, "priya@acme.example"); !strings.Contains(last[len(last)-1].Text, "marked as resolved") {
		t.Fatalf("auto-resolve notice missing: %s", last[len(last)-1].Text)
	}
	if f.category(t1) != "waitingCustomer" {
		t.Fatal("resolved before the auto-resolve delay")
	}

	// +49 h: T1 auto-resolved after the notice; T2 (resolved > 3 days ago)
	// archived.
	f.advance(49 * time.Hour)
	if f.category(t1) != "resolved" {
		t.Fatalf("T1: %s", f.category(t1))
	}
	if f.category(t2) != "archived" {
		t.Fatalf("T2: %s", f.category(t2))
	}
	var acts []string
	for _, it := range f.do("GET", tpath(t1, "/activity?limit=100"), nil, bearer(f.admin.Access)).expect(200).Items() {
		if it["action"] == "ticket.resolved" {
			acts = append(acts, obj(it, "actor")["type"].(string)+":"+it["reason"].(string))
		}
	}
	if len(acts) != 1 || !strings.HasPrefix(acts[0], "system:Automatically resolved") {
		t.Fatalf("auto-resolve audit: %v", acts)
	}
	// A resolved-by-automation ticket is archived after 3 more days.
	f.advance(73 * time.Hour)
	if f.category(t1) != "archived" {
		t.Fatalf("T1 not archived: %s", f.category(t1))
	}
}

func (f *ticketFixture) projectID() string {
	f.t.Helper()
	return f.do("GET", "/api/v1/project", nil, bearer(f.admin.Access)).expect(200).Str("id")
}

// ---- Live updates ----

type sseEvent struct {
	Event string
	Data  map[string]any
}

// openStream connects to the project's event stream and returns a channel
// of parsed events.
func openStream(t *testing.T, base, tok string) (<-chan sseEvent, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
	}
	ch := make(chan sseEvent, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.Data)
			case line == "" && ev.Event != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return ch, cancel
}

// collect gathers events until stop returns true or the timeout passes.
func collect(ch <-chan sseEvent, timeout time.Duration, stop func([]sseEvent) bool) []sseEvent {
	var out []sseEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
			if stop(out) {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

func invalidations(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		if e.Event == "invalidate" {
			out = append(out, e.Data["type"].(string)+"@"+e.Data["ticketId"].(string))
		}
	}
	return out
}

func TestLiveEventsOnlyForAccessibleTickets(t *testing.T) {
	f := newTicketFixture(t)
	f.c.LiveHeartbeat = 300 * time.Millisecond
	f.h = f.router(f.c)
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	srv := ts.URL

	// Tokens in the URL are never accepted.
	f.do("GET", "/api/v1/events?accessToken="+f.dana.Access, nil).expect(401)

	dana, stopDana := openStream(t, srv, f.dana.Access)
	defer stopDana()
	integ, stopInteg := openStream(t, srv, f.integration)
	defer stopInteg()
	for _, ch := range []<-chan sseEvent{dana, integ} {
		if evs := collect(ch, 5*time.Second, func(e []sseEvent) bool { return len(e) > 0 }); len(evs) == 0 || evs[0].Event != "ready" {
			t.Fatalf("no ready event: %v", evs)
		}
	}

	mine := f.createTicket(f.dana.Access, map[string]any{}).JSON()
	mineID := mine["id"].(string)
	hidden := f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery}).Str("id")
	f.do("POST", tpath(refOf(mine), "/messages"), map[string]any{"visibility": "agent", "body": "internal"}, bearer(f.dana.Access)).expect(201)
	f.do("POST", tpath(refOf(mine), "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(refOf(mine)))).expect(200)
	f.do("POST", tpath(refOf(mine), "/messages"), map[string]any{"visibility": "public", "body": "public reply"}, bearer(f.dana.Access)).expect(201)

	want := func(n int) func([]sseEvent) bool {
		return func(e []sseEvent) bool { return len(invalidations(e)) >= n }
	}
	got := invalidations(collect(dana, 10*time.Second, want(4)))
	if !slices.Equal(got, []string{"ticket.created@" + mineID, "message.created@" + mineID, "ticket.claimed@" + mineID, "message.created@" + mineID}) {
		t.Fatalf("agent events: %v", got)
	}
	// The integration sees public changes of every project ticket, but
	// nothing for the agent note or the claim, and never a staff version.
	evs := collect(integ, 10*time.Second, want(3))
	got = invalidations(evs)
	if !slices.Equal(got, []string{"ticket.created@" + mineID, "ticket.created@" + hidden, "message.created@" + mineID}) {
		t.Fatalf("integration events: %v", got)
	}
	for _, e := range evs {
		if e.Event == "invalidate" && e.Data["version"] != float64(0) {
			t.Fatalf("integration got a version: %v", e.Data)
		}
	}
	// A few more heartbeats must not add events about the hidden ticket.
	extra := invalidations(collect(dana, 1500*time.Millisecond, func([]sseEvent) bool { return false }))
	for _, e := range extra {
		if strings.HasSuffix(e, hidden) {
			t.Fatalf("hidden ticket announced to Dana: %v", extra)
		}
	}

	// Losing access (the account is disabled) ends the stream with an error event.
	v := f.do("GET", "/api/v1/users/"+f.dana.UserId, nil, bearer(f.admin.Access)).expect(200).Version()
	f.do("PATCH", "/api/v1/users/"+f.dana.UserId, map[string]any{"status": "disabled"}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	end := collect(dana, 5*time.Second, func(e []sseEvent) bool { return len(e) > 0 && e[len(e)-1].Event == "error" })
	if len(end) == 0 || end[len(end)-1].Event != "error" || end[len(end)-1].Data["code"] == "" {
		t.Fatalf("stream after removal: %v", end)
	}
}

func TestMCPSearchUsesAudienceScopedEngine(t *testing.T) {
	f := newMCPFixture(t)
	_, tok := f.integration("MCP search", "tickets:read")
	key := f.createTicket(f.dana.Access, map[string]any{"subject": "Webmail outage"}).Ref()
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "rootcause zanzibar in the cluster"}, bearer(f.dana.Access)).expect(201)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "We are looking into the outage"}, bearer(f.dana.Access)).expect(201)
	ig, agent := f.connect(tok), f.connect(f.dana.Access)

	// A word only in the agent note: found by staff, invisible to the
	// integration (no hit, no count, no snippet).
	if r := mustCall(t, agent, "ticketsSearch", map[string]any{"query": "zanzibar"}); r["total"] != float64(1) {
		t.Fatalf("agent search: %v", r)
	}
	r := mustCall(t, ig, "ticketsSearch", map[string]any{"query": "zanzibar"})
	if raw, _ := json.Marshal(r); r["total"] != float64(0) || len(r["items"].([]any)) != 0 || strings.Contains(string(raw), "zanzibar") {
		t.Fatalf("integration search leaked the note: %s", raw)
	}
	// Message words and snippets come from public messages.
	r = mustCall(t, ig, "ticketsSearch", map[string]any{"query": "looking"})
	items := r["items"].([]any)
	if r["total"] != float64(1) || len(items) != 1 || len(items[0].(map[string]any)["snippets"].([]any)) == 0 {
		t.Fatalf("public search: %v", r)
	}
	// A ticket number ("#12") is matched exactly.
	r = mustCall(t, agent, "ticketsSearch", map[string]any{"query": "#" + key})
	if refOf(obj(r, "exactMatch")) != key {
		t.Fatalf("key search: %v", r)
	}
}
