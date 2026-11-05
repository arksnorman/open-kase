package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/clock"
)

// slaFixture is a notification fixture whose ticket services, SLA timers
// and job runner run on a manual clock (Monday 2026-10-05 09:00 UTC).
// Authentication keeps the system clock, so tokens stay valid.
type slaFixture struct {
	*notifyFixture
	clk *clock.Manual
}

var slaStart = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC) // a Monday

func newSLAFixture(t *testing.T) *slaFixture {
	t.Helper()
	f := &slaFixture{notifyFixture: newNotifyFixture(t), clk: clock.NewManual(slaStart)}
	f.c.UseBusinessClock(f.clk)
	f.h = f.router(f.c)
	return f
}

// testPolicy: first response only, wall-clock 24/7. P1 30 min, P2 2 h,
// P3 8 h, P4 no target.
func testPolicy() map[string]any {
	return map[string]any{
		"warningPercent": 80,
		"targets": map[string]any{
			"1": map[string]any{"firstResponseMinutes": 30},
			"2": map[string]any{"firstResponseMinutes": 120},
			"3": map[string]any{"firstResponseMinutes": 480},
			"4": map[string]any{},
		},
	}
}

func (f *slaFixture) putPolicy() result {
	f.t.Helper()
	return f.do("PUT", "/api/v1/sla", testPolicy(), bearer(f.admin.Access)).expect(200)
}

// runDue dispatches pending outbox events and runs jobs that are due on
// the manual clock until nothing is left to do now.
func (f *ticketFixture) runDue() {
	f.t.Helper()
	ctx := context.Background()
	d := f.c.newDispatcher("test-dispatcher")
	r := f.c.newRunner("test-worker", 1)
	for i := 0; i < 1000; i++ {
		a, err := d.DispatchOnce(ctx)
		if err != nil {
			f.t.Fatalf("dispatch: %v", err)
		}
		b, err := r.RunOnce(ctx)
		if err != nil {
			f.t.Fatalf("run: %v", err)
		}
		if !a && !b {
			return
		}
	}
	f.t.Fatal("work did not settle")
}

func slaOf(tk map[string]any) map[string]any { v, _ := tk["sla"].(map[string]any); return v }

func target(s map[string]any, kind string) map[string]any {
	for _, raw := range s["targets"].([]any) {
		if t := raw.(map[string]any); t["kind"] == kind {
			return t
		}
	}
	return nil
}

func ts(t *testing.T, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("time %v: %v", v, err)
	}
	return at
}

func TestSLAOffUntilSaved(t *testing.T) {
	f := newSLAFixture(t)
	tk := f.createTicket(f.dana.Access, map[string]any{"priority": 1}).JSON()
	if tk["sla"] != nil {
		t.Fatalf("SLA without a policy: %v", tk["sla"])
	}
	// Before the first save GET returns the default: off, no targets.
	def := f.do("GET", "/api/v1/sla", nil, bearer(f.dana.Access)).expect(200).JSON()
	if def["active"] != false || def["version"] != float64(0) || def["warningPercent"] != float64(80) || def["updatedAt"] != nil ||
		obj(def, "targets")["1"].(map[string]any)["firstResponseMinutes"] != nil {
		t.Fatalf("default SLA %v", def)
	}
	f.do("PUT", "/api/v1/sla", testPolicy(), bearer(f.dana.Access)).expect(403)
	bad := testPolicy()
	obj(bad, "targets")["1"].(map[string]any)["firstResponseMinutes"] = 0
	f.do("PUT", "/api/v1/sla", bad, bearer(f.admin.Access)).expect(422)
	named := testPolicy()
	named["name"] = "Standard"
	f.do("PUT", "/api/v1/sla", named, bearer(f.admin.Access)).expect(422) // the SLA has no name

	p := f.putPolicy().JSON()
	if p["version"] != float64(1) || obj(p, "targets")["4"].(map[string]any)["firstResponseMinutes"] != nil {
		t.Fatalf("policy %v", p)
	}
	// Replacing needs If-Match; the version moves.
	f.do("PUT", "/api/v1/sla", testPolicy(), bearer(f.admin.Access)).expect(428)
	f.do("PUT", "/api/v1/sla", testPolicy(), bearer(f.admin.Access), ifMatchV(1)).expect(200)

	// The earlier ticket gets no SLA (a policy applies to new tickets only;
	// the backfill exists only for the demo seed) and there is no backfill
	// endpoint.
	if f.get(f.admin.Access, "1").expect(200).JSON()["sla"] != nil {
		t.Fatal("policy applied retroactively")
	}
	f.do("POST", "/api/v1/sla/backfill", nil, bearer(f.admin.Access)).expect(404)
	if res := f.backfillSLA(t); res.Started != 1 {
		t.Fatalf("backfill %+v", res)
	}
	s := slaOf(f.get(f.admin.Access, "1").expect(200).JSON())
	if s["kind"] != "firstResponse" || s["version"] != float64(2) || s["state"] != "ok" {
		t.Fatalf("backfilled sla %v", s)
	}
	// Integrations never see SLA data.
	if tk := f.get(f.integration, "1").expect(200).JSON(); tk["sla"] != nil || len(tk["tags"].([]any)) != 0 {
		t.Fatalf("integration view %v", tk)
	}
}

// slaCycles returns the ticket's SLA cycle history.
func (f *slaFixture) slaCycles(key string) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, c := range f.do("GET", tpath(key, "/sla"), nil, bearer(f.dana.Access)).expect(200).JSON()["cycles"].([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

// expectDue checks how many tickets the due filter lists.
func (f *slaFixture) expectDue(t *testing.T, due string, want int) {
	t.Helper()
	if got := keys(f.do("GET", "/api/v1/tickets?due="+due, nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != want {
		t.Fatalf("due=%s lists %v, want %d", due, got, want)
	}
}

// expectNoSLAWork runs everything due within the next days and fails if an
// SLA warning or breach is announced or an SLA timer stays queued.
func (f *slaFixture) expectNoSLAWork(t *testing.T) {
	t.Helper()
	before := f.outboxCount(t, outbox.TicketSLAWarning) + f.outboxCount(t, outbox.TicketSLABreached)
	f.clk.Advance(10 * 24 * time.Hour)
	f.runDue()
	if n := f.outboxCount(t, outbox.TicketSLAWarning) + f.outboxCount(t, outbox.TicketSLABreached); n != before {
		t.Fatalf("%d SLA events announced, want none", n-before)
	}
	n, err := f.coll("jobs").CountDocuments(context.Background(), bson.D{{Key: "kind", Value: sla.KindCheck}, {Key: "state", Value: string(jobs.StateQueued)}})
	if err != nil || n != 0 {
		t.Fatalf("%d SLA timers still queued (%v)", n, err)
	}
}

func TestSLAPrivateNoteDoesNotSatisfyFirstResponse(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	tk := f.createTicket(f.dana.Access, map[string]any{"priority": 1}).JSON()
	f.runDue() // the new-ticket notices
	s := slaOf(tk)
	if s["kind"] != "firstResponse" || s["state"] != "ok" || !ts(t, s["dueAt"]).Equal(slaStart.Add(30*time.Minute)) ||
		len(s["targets"].([]any)) != 1 {
		t.Fatalf("new ticket sla %v", s)
	}
	f.clk.Advance(10 * time.Minute)
	f.expectDue(t, "running", 1)
	f.expectDue(t, "dueSoon", 0)
	f.reply(f.dana.Access, "1", "agent", "Checking the MX records internally.")
	tk = f.get(f.dana.Access, "1").expect(200).JSON()
	if s := slaOf(tk); target(s, "firstResponse")["state"] != "running" || tk["firstResponseAt"] != nil {
		t.Fatalf("agent note satisfied the first response: %v", tk)
	}

	// 25 minutes: due soon, listed in the due-soon view and counted.
	f.clk.Set(slaStart.Add(25 * time.Minute))
	if s := slaOf(f.get(f.dana.Access, "1").JSON()); s["state"] != "dueSoon" {
		t.Fatalf("due soon expected: %v", s)
	}
	if got := keys(f.do("GET", "/api/v1/tickets?view=dueSoon", nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 1 {
		t.Fatalf("dueSoon view %v", got)
	}
	f.expectDue(t, "dueSoon", 1)
	f.expectDue(t, "running", 0)
	f.expectDue(t, "breached", 0)
	if c := f.do("GET", "/api/v1/queue-counts", nil, bearer(f.dana.Access)).expect(200).JSON(); c["dueSoon"] != float64(1) {
		t.Fatalf("queue counts %v", c)
	}
	f.mail.Reset()
	f.runDue()
	if n := f.outboxCount(t, outbox.TicketSLAWarning); n != 1 {
		t.Fatalf("%d warning events", n)
	}
	for _, who := range []string{"dana@example.com", "marco@example.com"} {
		got := sentTo(f.mail, who)
		if len(got) != 1 || !strings.Contains(got[0].Subject, "SLA due soon (first response)") {
			t.Fatalf("warning email for %s: %+v", who, got)
		}
	}
	if len(sentTo(f.mail, "priya@acme.example")) != 0 {
		t.Fatal("requester received an SLA email")
	}

	// 31 minutes: breached, announced once.
	f.clk.Set(slaStart.Add(31 * time.Minute))
	f.mail.Reset()
	f.runDue()
	f.runDue()
	if n := f.outboxCount(t, outbox.TicketSLABreached); n != 1 {
		t.Fatalf("%d breach events", n)
	}
	if got := sentTo(f.mail, "marco@example.com"); len(got) != 1 || !strings.Contains(got[0].Subject, "SLA missed") {
		t.Fatalf("breach email %+v", got)
	}
	if s := slaOf(f.get(f.dana.Access, "1").JSON()); s["state"] != "breached" || s["breached"] != true {
		t.Fatalf("breached state %v", s)
	}
	f.expectDue(t, "breached", 1)
	f.expectDue(t, "dueSoon", 0)

	// The first public staff reply meets it late: the breach stays recorded
	// and the cycle closes as met.
	f.clk.Set(slaStart.Add(40 * time.Minute))
	f.reply(f.dana.Access, "1", "public", "We are on it.")
	tk = f.get(f.dana.Access, "1").JSON()
	s = slaOf(tk)
	fr := target(s, "firstResponse")
	if fr["state"] != "met" || fr["breachedAt"] == nil || s["kind"] != "firstResponse" || s["state"] != "met" || s["breached"] != true ||
		!ts(t, tk["firstResponseAt"]).Equal(slaStart.Add(40*time.Minute)) {
		t.Fatalf("after reply %v", tk)
	}
	cycles := f.slaCycles("1")
	if len(cycles) != 1 || len(cycles[0]["breaches"].([]any)) != 1 || cycles[0]["closedReason"] != "met" || cycles[0]["current"] != true {
		t.Fatalf("history %v", cycles)
	}
	f.expectDue(t, "breached", 0)
	f.expectDue(t, "missed", 1)
	f.expectDue(t, "met", 0)
	f.do("GET", tpath("1", "/sla"), nil, bearer(f.integration)).expect(403, "userRequired")

	// A requester reply after that starts nothing.
	f.clk.Advance(time.Minute)
	f.reply(f.integration, "1", "public", "Thanks, still broken though.")
	if s := slaOf(f.get(f.dana.Access, "1").JSON()); s["state"] != "met" || len(f.slaCycles("1")) != 1 {
		t.Fatalf("requester reply after met %v", s)
	}
	f.expectNoSLAWork(t)
}

func TestSLAFirstPublicReplyMeetsAndClosesTheCycle(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.createTicket(f.integration, map[string]any{}) // P3: 8 hours
	f.clk.Advance(5 * time.Minute)
	// A requester follow-up before any staff answer changes nothing.
	f.reply(f.integration, "1", "public", "Any news?")
	if s := slaOf(f.get(f.dana.Access, "1").JSON()); s["state"] != "ok" || s["kind"] != "firstResponse" {
		t.Fatalf("after a requester reply %v", s)
	}
	f.clk.Advance(15 * time.Minute)
	tk := f.reply(f.dana.Access, "1", "public", "Looking into it.").JSON()
	if s := slaOf(obj(tk, "ticket")); s == nil || s["state"] != "met" || s["breached"] != false || s["dueAt"] != nil {
		t.Fatalf("reply response sla %v", obj(tk, "ticket")["sla"])
	}
	f.expectDue(t, "met", 1)
	f.expectDue(t, "missed", 0)
	cycles := f.slaCycles("1")
	if len(cycles) != 1 || cycles[0]["closedReason"] != "met" || cycles[0]["closedAt"] == nil {
		t.Fatalf("cycle after the first reply %v", cycles)
	}
	if got := keys(f.do("GET", "/api/v1/tickets?view=dueSoon", nil, bearer(f.dana.Access)).Items()); len(got) != 0 {
		t.Fatalf("met ticket due soon %v", got)
	}
	f.expectNoSLAWork(t)
}

func TestSLAAgentTokenPublicReplyMeetsFirstResponse(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	_, bot := f.token("Support bot", "tickets:read", "tickets:comment", "tickets:internal")
	f.clk.Advance(5 * time.Minute)
	f.reply(bot, "1", "agent", "Bot note: checked DNS.")
	if s := slaOf(f.get(f.dana.Access, "1").JSON()); s["state"] != "ok" {
		t.Fatalf("a bot note met the first response: %v", s)
	}
	f.clk.Advance(5 * time.Minute)
	f.reply(bot, "1", "public", "Please retry now.")
	if s := slaOf(f.get(f.dana.Access, "1").JSON()); s["state"] != "met" || s["kind"] != "firstResponse" {
		t.Fatalf("a bot public reply did not meet the first response: %v", s)
	}
	f.expectNoSLAWork(t)
}

func TestSLAPriorityRecalculationAndNoWaitingPause(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 2})
	// P2: 2 hours from Mon 09:00 → 11:00.
	s := slaOf(f.get(f.dana.Access, "1").JSON())
	if !ts(t, s["dueAt"]).Equal(slaStart.Add(2 * time.Hour)) {
		t.Fatalf("P2 first response due %v", s)
	}
	// Waiting for the customer pauses nothing.
	f.clk.Set(slaStart.Add(30 * time.Minute))
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "setStatus", "statusId": f.waitingStatusID()},
		bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	if s := slaOf(f.get(f.dana.Access, "1").JSON()); s["state"] != "ok" || !ts(t, s["dueAt"]).Equal(slaStart.Add(2*time.Hour)) {
		t.Fatalf("waiting changed the SLA %v", s)
	}
	// Raise to P1 at 10:00 (30 min): 60 minutes already elapsed, so
	// it is breached at once.
	f.clk.Set(slaStart.Add(time.Hour))
	tk := f.do("PATCH", tpath("1"), map[string]any{"priority": 1}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200).JSON()
	s = slaOf(tk)
	fr := target(s, "firstResponse")
	if fr["elapsedSeconds"] != float64(60*60) || fr["budgetSeconds"] != float64(30*60) || fr["breachedAt"] == nil || s["state"] != "breached" {
		t.Fatalf("P1 first response %v", s)
	}
	// Lowering the priority again does not erase the breach.
	f.clk.Advance(time.Minute)
	s = slaOf(f.do("PATCH", tpath("1"), map[string]any{"priority": 3}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200).JSON())
	if target(s, "firstResponse")["breachedAt"] == nil || s["breached"] != true {
		t.Fatalf("breach erased by downgrade: %v", s)
	}
}

func (f *ticketFixture) waitingStatusID() string {
	f.t.Helper()
	for _, st := range f.do("GET", "/api/v1/statuses", nil, bearer(f.admin.Access)).expect(200).Items() {
		if st["category"] == "waitingCustomer" {
			return st["id"].(string)
		}
	}
	f.t.Fatal("no waiting status")
	return ""
}

// The clock runs 24/7: a ticket created on Friday evening is due two hours
// later, on Friday night.
func TestSLARunsAroundTheClock(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.clk.Set(time.Date(2026, 10, 9, 23, 0, 0, 0, time.UTC))
	tk := f.createTicket(f.dana.Access, map[string]any{"priority": 2}).JSON()
	if fr := target(slaOf(tk), "firstResponse"); !ts(t, fr["dueAt"]).Equal(time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("P2 due %v", fr)
	}
}

func TestSLAResolveAndReopenStartNoSLA(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.createTicket(f.integration, map[string]any{}) // P3 (requester side)
	f.clk.Advance(20 * time.Minute)
	// Resolved before any staff reply: the first response stops, unmet.
	tk := f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access),
		ifMatchV(f.version("1"))).expect(200).JSON()
	if tk["sla"] != nil {
		t.Fatalf("resolved before a reply still shows SLA %v", tk["sla"])
	}
	if c := f.slaCycles("1"); len(c) != 1 || c[0]["closedReason"] != "resolved" || target(c[0], "firstResponse")["state"] != "stopped" {
		t.Fatalf("history after resolve %v", c)
	}
	// The requester reopens with a reply: no new cycle, no SLA.
	f.clk.Advance(time.Hour)
	f.reply(f.integration, "1", "public", "It broke again.")
	tk = f.get(f.dana.Access, "1").JSON()
	if tk["sla"] != nil || obj(tk, "status")["category"] == "resolved" || len(f.slaCycles("1")) != 1 {
		t.Fatalf("reopened ticket %v", tk)
	}
	if got := keys(f.do("GET", "/api/v1/tickets?view=dueSoon", nil, bearer(f.dana.Access)).Items()); len(got) != 0 {
		t.Fatalf("reopened ticket due soon %v", got)
	}
	// The reopen is recorded for the reports.
	n, _ := f.coll("auditEvents").CountDocuments(context.Background(), bson.D{{Key: "action", Value: "ticket.reopened"}})
	if n != 1 {
		t.Fatalf("%d reopen audit events", n)
	}
	// A staff reply now changes nothing either (the cycle is closed).
	f.reply(f.dana.Access, "1", "public", "Fixed again.")
	if tk := f.get(f.dana.Access, "1").JSON(); tk["sla"] != nil || tk["firstResponseAt"] == nil {
		t.Fatalf("staff reply after reopen %v", tk)
	}
	f.expectNoSLAWork(t)

	// A met first response stays met through resolve and an agent reopen.
	f.createTicket(f.integration, map[string]any{})
	f.reply(f.dana.Access, "2", "public", "Done.")
	f.do("POST", tpath("2", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version("2"))).expect(200)
	s := slaOf(f.do("POST", tpath("2", "/transitions"), map[string]any{"action": "reopen"}, bearer(f.dana.Access),
		ifMatchV(f.version("2"))).expect(200).JSON())
	if s == nil || s["state"] != "met" || s["cycle"] != float64(1) || len(f.slaCycles("2")) != 1 {
		t.Fatalf("agent reopen %v", s)
	}
	f.expectNoSLAWork(t)
}

func TestSLAWebhookEvents(t *testing.T) {
	f := newSLAFixture(t)
	f.c.Boot.Env = EnvDevelopment // plain http to the local receiver
	hs := newHookServer(t)
	f.do("POST", "/api/v1/webhooks", map[string]any{"name": "Test hook", "url": hs.URL + "/hook",
		"eventTypes": []string{"ticket.slaWarning", "ticket.slaBreached"}}, bearer(f.admin.Access)).expect(201)
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	f.clk.Set(slaStart.Add(31 * time.Minute))
	f.runDue()
	got := hs.all()
	if len(got) != 1 {
		t.Fatalf("%d webhook deliveries", len(got))
	}
	var env struct {
		Type string `json:"type"`
		Data struct {
			SLA struct {
				Kind  string    `json:"kind"`
				State string    `json:"state"`
				DueAt time.Time `json:"dueAt"`
			} `json:"sla"`
			Priority int `json:"priority"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &env); err != nil || env.Type != "ticket.slaBreached" || env.Data.SLA.Kind != "firstResponse" ||
		env.Data.SLA.State != "breached" || !env.Data.SLA.DueAt.Equal(slaStart.Add(30*time.Minute)) || env.Data.Priority != 1 {
		t.Fatalf("payload %s (%v)", got[0].body, err)
	}
}

func TestSLABreachJobIsFenced(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	ctx := context.Background()
	// Past the deadline: the pending timer (scheduled for the warning time)
	// is due. Worker A takes it and stalls; its lease expires and worker B
	// takes over and completes.
	now := slaStart.Add(31 * time.Minute)
	f.clk.Set(now)
	kinds := []string{sla.KindCheck}
	jA, ok, err := f.c.Jobs.Acquire(ctx, "worker-a", kinds, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire A: %v %v", ok, err)
	}
	jB, ok, err := f.c.Jobs.Acquire(ctx, "worker-b", kinds, now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || jB.Id != jA.Id || jB.Generation != jA.Generation+1 {
		t.Fatalf("takeover: %+v %v %v", jB, ok, err)
	}
	handler := sla.CheckHandler(f.c.Tickets)
	complete := func(j jobs.Job, owner string) error {
		l := jobs.Lease{JobId: j.Id, Owner: owner, Generation: j.Generation}
		res := handler(ctx, j, l)
		fin := jobs.Finish{State: jobs.StateSucceeded, Attempt: jobs.Attempt{Number: j.Attempts, Owner: owner, Generation: j.Generation, Outcome: "succeeded"}}
		return f.c.Jobs.Complete(ctx, l, fin, func(ctx context.Context) error { return res.Commit(ctx, fin) })
	}
	// The stale worker finishes first: its completion is fenced out and
	// none of its writes (breach record, outbox event, next timer) commit.
	if err := complete(jA, "worker-a"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale completion: %v", err)
	}
	if n := f.outboxCount(t, outbox.TicketSLABreached); n != 0 {
		t.Fatalf("stale worker committed %d breach events", n)
	}
	if err := complete(jB, "worker-b"); err != nil {
		t.Fatalf("complete B: %v", err)
	}
	if n := f.outboxCount(t, outbox.TicketSLABreached); n != 1 {
		t.Fatalf("%d breach events, want exactly 1", n)
	}
	// A timer for an outdated generation does nothing even with a valid
	// lease: the cycle moved on when B announced the breach.
	gen, _ := strconv.ParseInt(jA.Payload["generation"], 10, 64)
	if err := f.c.Tickets.CheckSLA(ctx, jA.Payload["instanceId"], gen); err != nil {
		t.Fatal(err)
	}
	if n := f.outboxCount(t, outbox.TicketSLABreached) + f.outboxCount(t, outbox.TicketSLAWarning); n != 1 {
		t.Fatalf("stale generation emitted events: %d", n)
	}
}

// backfillSLA runs the seed-only SLA backfill as the admin.
func (f *slaFixture) backfillSLA(t *testing.T) tickets.BackfillResult {
	t.Helper()
	scope, err := f.c.Access.Scope(context.Background(), f.principal(t, f.admin))
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.c.Tickets.BackfillSLA(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
