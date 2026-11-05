package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
)

// P-SLA-1 (owner-approved follow-up): turning the SLA off turns SLA
// off for open tickets. Before, the running cycles went on: their timers
// still announced warnings and breaches by email and the tickets kept a
// running SLA, the due-soon queue and the due filters.

// setPolicyActive saves the test policy with active set, replacing version.
func (f *slaFixture) setPolicyActive(active bool, version int64) {
	f.t.Helper()
	p := testPolicy()
	p["active"] = active
	f.do("PUT", "/api/v1/sla", p, bearer(f.admin.Access), ifMatchV(version)).expect(200)
}

// slaHistory returns a ticket's SLA cycles (oldest first) and its current
// SLA from GET .../sla.
func (f *slaFixture) slaHistory(key string) ([]map[string]any, map[string]any) {
	f.t.Helper()
	h := f.do("GET", tpath(key, "/sla"), nil, bearer(f.admin.Access)).expect(200).JSON()
	var cycles []map[string]any
	for _, c := range h["cycles"].([]any) {
		cycles = append(cycles, c.(map[string]any))
	}
	cur, _ := h["current"].(map[string]any)
	return cycles, cur
}

// slaMail returns the SLA alert emails sent since the last reset.
func (f *slaFixture) slaMail() []string {
	var out []string
	for _, m := range f.mail.Sent() {
		if strings.Contains(m.Subject, "SLA") {
			out = append(out, m.To.Email+": "+m.Subject)
		}
	}
	return out
}

func (f *slaFixture) slaEvents(t *testing.T) int64 {
	return f.outboxCount(t, outbox.TicketSLAWarning) + f.outboxCount(t, outbox.TicketSLABreached)
}

func (f *slaFixture) reportBreaches() map[string]any {
	f.t.Helper()
	r := f.do("GET", "/api/v1/reports/summary?from=2026-10-05&to=2026-10-05", nil, bearer(f.admin.Access)).expect(200).JSON()
	return obj(r, "slaBreaches")
}

// offWithBreach: #1 (P1, created at 09:00) misses its first response
// at 09:30 (announced at 09:31), #2 (P1) is created at 09:31, and SLA
// is turned off at 09:40 (policy version 2).
func (f *slaFixture) offWithBreach(t *testing.T) {
	f.t.Helper()
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	f.runDue()
	f.clk.Set(slaStart.Add(31 * time.Minute))
	f.runDue()
	if n := f.outboxCount(t, outbox.TicketSLABreached); n != 1 {
		t.Fatalf("%d breach events before the deactivation, want 1", n)
	}
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	f.runDue()
	f.clk.Set(slaStart.Add(40 * time.Minute))
	f.setPolicyActive(false, 1)
}

func TestSLAOffStopsRunningCycles(t *testing.T) {
	f := newSLAFixture(t)
	f.offWithBreach(t)
	before := f.slaEvents(t)
	f.mail.Reset()

	// Hours later every warning and deadline of both tickets has passed:
	// nothing is announced.
	f.clk.Set(slaStart.Add(5 * time.Hour))
	f.runDue()
	if n := f.slaEvents(t); n != before {
		t.Fatalf("%d SLA events after SLA was turned off", n-before)
	}
	if got := f.slaMail(); len(got) != 0 {
		t.Fatalf("SLA emails after SLA was turned off: %v", got)
	}

	// No ticket shows a running SLA, in no view or filter.
	for _, key := range []string{"1", "2"} {
		if s := f.get(f.dana.Access, key).expect(200).JSON()["sla"]; s != nil {
			t.Fatalf("%s still shows an SLA: %v", key, s)
		}
		if _, cur := f.slaHistory(key); cur != nil {
			t.Fatalf("%s history has a current SLA: %v", key, cur)
		}
	}
	for _, q := range []string{"view=dueSoon", "due=dueSoon", "due=breached"} {
		if got := keys(f.do("GET", "/api/v1/tickets?"+q, nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 0 {
			t.Fatalf("%s lists %v", q, got)
		}
	}
	if c := f.do("GET", "/api/v1/queue-counts", nil, bearer(f.dana.Access)).expect(200).JSON(); c["dueSoon"] != float64(0) {
		t.Fatalf("queue counts %v", c)
	}

	// The cycles were stopped at the deactivation time with the reason;
	// the breach recorded before stays, nothing after it is recorded.
	off := slaStart.Add(40 * time.Minute)
	for key, breaches := range map[string]int{"1": 1, "2": 0} {
		cycles, _ := f.slaHistory(key)
		if len(cycles) != 1 {
			t.Fatalf("%s cycles %v", key, cycles)
		}
		c := cycles[0]
		if c["current"] != false || c["closedReason"] != "slaOff" || !ts(t, c["closedAt"]).Equal(off) ||
			len(c["breaches"].([]any)) != breaches {
			t.Fatalf("%s stopped cycle %v", key, c)
		}
		for _, raw := range c["targets"].([]any) {
			if tg := raw.(map[string]any); tg["state"] != "stopped" && tg["state"] != "met" {
				t.Fatalf("%s target after the stop %v", key, tg)
			}
		}
	}
	cycles, _ := f.slaHistory("1")
	if b := cycles[0]["breaches"].([]any)[0].(map[string]any); b["kind"] != "firstResponse" || !ts(t, b["at"]).Equal(slaStart.Add(30*time.Minute)) {
		t.Fatalf("recorded breach %v", b)
	}
	// Reports count the recorded breach only.
	if r := f.reportBreaches(); r["firstResponse"] != float64(1) || len(r) != 1 {
		t.Fatalf("report breaches %v", r)
	}
	n, _ := f.coll("auditEvents").CountDocuments(context.Background(), bson.D{{Key: "action", Value: "sla.cyclesStopped"},
		{Key: "changes", Value: "stopped:2"}})
	if n != 1 {
		t.Fatalf("%d sla.cyclesStopped audit events", n)
	}
}

func TestSLAOnAgainBackfillRestartsStoppedTickets(t *testing.T) {
	f := newSLAFixture(t)
	f.offWithBreach(t)
	f.clk.Set(slaStart.Add(5 * time.Hour))
	f.runDue()

	// On again (version 3): not retroactive, a new ticket gets a cycle.
	f.setPolicyActive(true, 2)
	if s := f.get(f.admin.Access, "1").expect(200).JSON()["sla"]; s != nil {
		t.Fatalf("reactivation restarted #1 by itself: %v", s)
	}
	if s := slaOf(f.createTicket(f.dana.Access, map[string]any{"priority": 1}).JSON()); s == nil || s["cycle"] != float64(1) || s["version"] != float64(3) {
		t.Fatalf("new ticket after reactivation %v", s)
	}

	// The backfill starts new cycles for the stopped tickets. They start
	// now: the time SLA was off does not count and the first response is
	// still owed with its full budget.
	now := f.clk.Now()
	if r := f.backfillSLA(t); r.Started != 2 || r.More {
		t.Fatalf("backfill %+v", r)
	}
	for _, key := range []string{"1", "2"} {
		s := slaOf(f.get(f.dana.Access, key).expect(200).JSON())
		if s == nil || s["cycle"] != float64(2) || s["version"] != float64(3) || s["kind"] != "firstResponse" ||
			s["state"] != "ok" || s["breached"] != false || !ts(t, s["dueAt"]).Equal(now.Add(30*time.Minute)) {
			t.Fatalf("%s backfilled sla %v", key, s)
		}
	}
	cycles, cur := f.slaHistory("1")
	if len(cycles) != 2 || cur == nil || cycles[0]["closedReason"] != "slaOff" || len(cycles[0]["breaches"].([]any)) != 1 ||
		cycles[1]["current"] != true || len(cycles[1]["breaches"].([]any)) != 0 {
		t.Fatalf("history after the backfill %v", cycles)
	}
	// The breach of the stopped cycle is not recorded a second time.
	if b := f.reportBreaches(); b["firstResponse"] != float64(1) {
		t.Fatalf("report breaches after the backfill %v", b)
	}
	// A second backfill finds nothing to start.
	if r := f.backfillSLA(t); r.Started != 0 {
		t.Fatalf("second backfill %+v", r)
	}

	// SLA is on again: the restarted cycles announce their deadlines.
	before := f.outboxCount(t, outbox.TicketSLABreached)
	f.clk.Set(now.Add(31 * time.Minute))
	f.runDue()
	if n := f.outboxCount(t, outbox.TicketSLABreached) - before; n != 3 {
		t.Fatalf("%d breach events after SLA was turned on again, want 3", n)
	}
}

// runTimersOnly runs one due SLA timer job (and nothing else), as a worker
// that takes a due timer before the stop job does.
func (f *slaFixture) runTimersOnly(t *testing.T) {
	t.Helper()
	r := jobs.NewRunner(f.c.Jobs, jobs.Config{Owner: "timer-worker", Now: f.clk.Now})
	r.Handle(sla.KindCheck, sla.CheckHandler(f.c.Tickets))
	if ok, err := r.RunOnce(context.Background()); err != nil || !ok {
		t.Fatalf("no due SLA timer ran: %v %v", ok, err)
	}
}

func TestSLAOffTimerRacingTheDeactivationSendsNothing(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	f.runDue()
	// 09:25: the first-response warning (09:24) is due, but SLA is turned
	// off before a worker takes the timer; the timer runs after that
	// commit and before the stop job.
	f.clk.Set(slaStart.Add(25 * time.Minute))
	f.setPolicyActive(false, 1)
	f.mail.Reset()
	f.runTimersOnly(t)
	if n := f.slaEvents(t); n != 0 {
		t.Fatalf("a timer racing the deactivation emitted %d SLA events", n)
	}
	// The timer stopped the cycle itself.
	if s := f.get(f.dana.Access, "1").expect(200).JSON()["sla"]; s != nil {
		t.Fatalf("#1 still shows an SLA: %v", s)
	}
	cycles, _ := f.slaHistory("1")
	if len(cycles) != 1 || cycles[0]["closedReason"] != "slaOff" || !ts(t, cycles[0]["closedAt"]).Equal(slaStart.Add(25*time.Minute)) {
		t.Fatalf("cycle after the racing timer %v", cycles)
	}
	f.clk.Set(slaStart.Add(2 * time.Hour))
	f.runDue()
	if n := f.slaEvents(t); n != 0 {
		t.Fatalf("%d SLA events after SLA was turned off", n)
	}
	if got := f.slaMail(); len(got) != 0 {
		t.Fatalf("SLA emails after SLA was turned off: %v", got)
	}
}

// Off and on again before the stop job ran: the old cycle's timer still
// announces nothing (it started before the deactivation) and the stop job
// leaves the cycles started after the reactivation running.
func TestSLAOffAndOnAgainBeforeTheStopJobRuns(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	f.runDue()
	f.clk.Set(slaStart.Add(25 * time.Minute))
	f.setPolicyActive(false, 1)
	f.setPolicyActive(true, 2)
	f.createTicket(f.dana.Access, map[string]any{"priority": 1}) // #2, cycle of version 3
	f.runTimersOnly(t)
	if n := f.slaEvents(t); n != 0 {
		t.Fatalf("the timer of a cycle older than the deactivation emitted %d SLA events", n)
	}
	f.runDue()
	if s := f.get(f.dana.Access, "1").expect(200).JSON()["sla"]; s != nil {
		t.Fatalf("#1 still shows an SLA: %v", s)
	}
	if s := slaOf(f.get(f.dana.Access, "2").expect(200).JSON()); s == nil || s["version"] != float64(3) {
		t.Fatalf("the stop job stopped a cycle started after the reactivation: %v", s)
	}
	f.clk.Set(slaStart.Add(57 * time.Minute)) // #2's first response was due at 09:55
	f.runDue()
	if n := f.outboxCount(t, outbox.TicketSLABreached); n != 1 {
		t.Fatalf("%d breach events, want #2's only", n)
	}
}
