package sla

import (
	"testing"
	"time"
)

func at(t *testing.T, v string) time.Time {
	t.Helper()
	x, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func snapshot() Snapshot {
	s := Snapshot{PolicyId: "p1", PolicyVersion: 3, WarningPercent: 80}
	s.Targets[0] = Targets{FirstResponseMinutes: 30}
	s.Targets[1] = Targets{FirstResponseMinutes: 120}
	s.Targets[2] = Targets{FirstResponseMinutes: 480}
	s.Targets[3] = Targets{FirstResponseMinutes: 1440}
	return s
}

func due(t *testing.T, in *Instance, k Kind) time.Time {
	t.Helper()
	c := in.Clock(k)
	if c == nil || c.DueAt == nil {
		t.Fatalf("%s has no deadline: %+v", k, c)
	}
	return *c.DueAt
}

func TestStartRunsOnlyTheFirstResponseClock(t *testing.T) {
	in := Start(snapshot(), 1, 1, at(t, "2026-10-05T09:00:00Z"), StartOptions{})
	if len(in.Clocks) != 1 || in.Clocks[0].Kind != FirstResponse || in.Clocks[0].State != Running {
		t.Fatalf("clocks %+v", in.Clocks)
	}
}

func TestFirstResponseMetClosesTheCycle(t *testing.T) {
	t0 := at(t, "2026-10-05T09:00:00Z") // Monday
	in := Start(snapshot(), 1, 1, t0, StartOptions{})
	if got := due(t, &in, FirstResponse); !got.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("P1 first response due %v", got)
	}
	if w := in.Clock(FirstResponse).WarnAt; w == nil || !w.Equal(t0.Add(24*time.Minute)) {
		t.Fatalf("warning at %v", w)
	}
	if s := in.Summarize(t0.Add(10 * time.Minute)); s.Kind != FirstResponse || s.State != SummaryOK {
		t.Fatalf("summary %+v", s)
	}
	if s := in.Summarize(t0.Add(25 * time.Minute)); s.State != SummaryDueSoon {
		t.Fatalf("due soon expected: %+v", s)
	}
	gen := in.Generation
	in.StaffPublicReply(t0.Add(20 * time.Minute))
	fr := in.Clock(FirstResponse)
	if fr.State != Met || fr.MetAt == nil || fr.BreachedAt != nil || len(in.Breaches) != 0 {
		t.Fatalf("met clock %+v", fr)
	}
	if !in.Closed() || !in.Current || in.ClosedReason != ClosedMet || in.Generation == gen {
		t.Fatalf("cycle after the reply %+v", in)
	}
	if s := in.Summarize(t0.Add(2 * time.Hour)); s.State != SummaryMet || s.Kind != FirstResponse || s.Breached {
		t.Fatalf("met summary %+v", s)
	}
	if due, warn := in.Projection(); due != nil || warn != nil || in.NextCheck() != nil || len(in.Check(t0.Add(5*time.Hour))) != 0 {
		t.Fatal("a met cycle still projects, schedules or announces")
	}
	// Nothing afterwards changes it: another reply, a priority change, a
	// resolve.
	gen = in.Generation
	in.StaffPublicReply(t0.Add(time.Hour))
	in.ChangePriority(4, t0.Add(time.Hour))
	in.Resolve(t0.Add(2 * time.Hour))
	if in.Generation != gen || in.ClosedReason != ClosedMet || in.Priority != 1 {
		t.Fatalf("closed cycle changed %+v", in)
	}
}

func TestLateFirstResponseKeepsTheBreach(t *testing.T) {
	t0 := at(t, "2026-10-05T09:00:00Z")
	in := Start(snapshot(), 1, 1, t0, StartOptions{})
	// An agent note is not reported to the engine at all; at 31 minutes the
	// first response is breached.
	if s := in.Summarize(t0.Add(31 * time.Minute)); s.State != SummaryBreached || !s.Breached {
		t.Fatalf("breach expected: %+v", s)
	}
	in.StaffPublicReply(t0.Add(40 * time.Minute))
	fr := in.Clock(FirstResponse)
	if fr.State != Met || fr.BreachedAt == nil || !fr.BreachedAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("late reply must keep the breach: %+v", fr)
	}
	if len(in.Breaches) != 1 || in.Breaches[0].Kind != FirstResponse || in.ClosedReason != ClosedMet {
		t.Fatalf("breach history %+v", in.Breaches)
	}
	if s := in.Summarize(t0.Add(41 * time.Minute)); s.State != SummaryMet || !s.Breached {
		t.Fatalf("summary after the late reply %+v", s)
	}
}

func TestPriorityChangeRecalculatesRemainingBudget(t *testing.T) {
	t0 := at(t, "2026-10-05T09:00:00Z")
	in := Start(snapshot(), 3, 1, t0, StartOptions{})
	// One hour elapsed, then raise to P2 (2 h): one hour remains.
	t1 := t0.Add(time.Hour)
	in.ChangePriority(2, t1)
	fr := in.Clock(FirstResponse)
	if fr.ElapsedSeconds != 3600 || fr.BudgetSeconds != 120*60 {
		t.Fatalf("elapsed/budget %+v", fr)
	}
	if got := due(t, &in, FirstResponse); !got.Equal(t1.Add(time.Hour)) {
		t.Fatalf("P2 due %v", got)
	}
	// Raise to P1 (30 min) after 1 h 30 elapsed: breached at once.
	t2 := t0.Add(90 * time.Minute)
	in.ChangePriority(1, t2)
	fr = in.Clock(FirstResponse)
	if fr.BreachedAt == nil || !fr.BreachedAt.Equal(t2) {
		t.Fatalf("expected an immediate breach: %+v", fr)
	}
	// Lowering to P4 never erases the breach.
	in.ChangePriority(4, t2.Add(time.Minute))
	fr = in.Clock(FirstResponse)
	if fr.BreachedAt == nil || len(in.Breaches) != 1 {
		t.Fatalf("breach erased: %+v %+v", fr, in.Breaches)
	}
	if s := in.Summarize(t2.Add(2 * time.Minute)); !s.Breached || s.State != SummaryBreached {
		t.Fatalf("summary after downgrade %+v", s)
	}
}

func TestResolveOrArchiveBeforeAReplyStops(t *testing.T) {
	t0 := at(t, "2026-10-05T09:00:00Z")
	for _, tc := range []struct {
		reason string
		end    func(*Instance, time.Time)
	}{{ClosedResolved, (*Instance).Resolve}, {ClosedArchived, (*Instance).Archive}} {
		in := Start(snapshot(), 1, 1, t0, StartOptions{})
		tc.end(&in, t0.Add(10*time.Minute))
		if !in.Closed() || !in.Current || in.ClosedReason != tc.reason || in.Clock(FirstResponse).State != Stopped {
			t.Fatalf("%s: %+v", tc.reason, in)
		}
		if s := in.Summarize(t0.Add(2 * time.Hour)); s != nil {
			t.Fatalf("%s: a stopped first response shows a summary %+v", tc.reason, s)
		}
		if in.NextCheck() != nil {
			t.Fatalf("%s: closed cycle scheduled a check", tc.reason)
		}
		gen := in.Generation
		in.StaffPublicReply(t0.Add(3 * time.Hour)) // after reopen: ignored
		if in.Generation != gen || in.Clock(FirstResponse).State != Stopped {
			t.Fatalf("%s: closed cycle changed", tc.reason)
		}
	}
}

func TestCheckAnnouncesWarningThenBreachOnce(t *testing.T) {
	t0 := at(t, "2026-10-05T09:00:00Z")
	in := Start(snapshot(), 1, 1, t0, StartOptions{})
	next := in.NextCheck()
	if next == nil || !next.Equal(t0.Add(24*time.Minute)) {
		t.Fatalf("first check at %v", next)
	}
	if n := in.Check(t0.Add(10 * time.Minute)); len(n) != 0 {
		t.Fatalf("early check announced %+v", n)
	}
	n := in.Check(t0.Add(24 * time.Minute))
	if len(n) != 1 || n[0].Breached || n[0].Kind != FirstResponse {
		t.Fatalf("warning %+v", n)
	}
	if n := in.Check(t0.Add(25 * time.Minute)); len(n) != 0 {
		t.Fatalf("warning repeated %+v", n)
	}
	if next := in.NextCheck(); next == nil || !next.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("breach check at %v", next)
	}
	n = in.Check(t0.Add(31 * time.Minute))
	if len(n) != 1 || !n[0].Breached || !n[0].DueAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("breach %+v", n)
	}
	if n := in.Check(t0.Add(32 * time.Minute)); len(n) != 0 {
		t.Fatalf("breach repeated %+v", n)
	}
	if in.NextCheck() != nil {
		t.Fatal("a timer is scheduled for an announced breach")
	}
}

func TestNoTargetsForPriorityMeansNoSummary(t *testing.T) {
	snap := snapshot()
	snap.Targets[3] = Targets{}
	in := Start(snap, 4, 1, at(t, "2026-10-05T09:00:00Z"), StartOptions{})
	if s := in.Summarize(at(t, "2026-10-05T10:00:00Z")); s != nil {
		t.Fatalf("summary without targets: %+v", s)
	}
	if in.NextCheck() != nil {
		t.Fatal("check scheduled without targets")
	}
}

func TestDeactivateStopsTheCycleAndKeepsBreaches(t *testing.T) {
	t0 := at(t, "2026-10-05T09:00:00Z")
	in := Start(snapshot(), 1, 1, t0, StartOptions{})
	in.Check(t0.Add(31 * time.Minute)) // first response breached at 09:30
	gen := in.Generation
	in.Deactivate(t0.Add(40 * time.Minute))
	if in.Current || !in.Closed() || !in.ClosedAt.Equal(t0.Add(40*time.Minute)) || in.ClosedReason != ClosedDeactivated {
		t.Fatalf("deactivated cycle %+v", in)
	}
	if in.Generation == gen {
		t.Fatal("deactivation must invalidate pending timers")
	}
	for _, c := range in.Clocks {
		if c.State != Stopped || c.DueAt != nil {
			t.Fatalf("clock after the stop %+v", c)
		}
	}
	if len(in.Breaches) != 1 || in.Breaches[0].Kind != FirstResponse || in.Clock(FirstResponse).BreachedAt == nil {
		t.Fatalf("breach history %+v", in.Breaches)
	}
	if due, warn := in.Projection(); due != nil || warn != nil || in.NextCheck() != nil || len(in.Check(t0.Add(5*time.Hour))) != 0 {
		t.Fatal("a stopped cycle still projects, schedules or announces")
	}

	// A deadline that passed before the stop is recorded by it (not
	// announced); a closed cycle (met or resolved) is left as it is.
	late := Start(snapshot(), 1, 1, t0, StartOptions{})
	late.Deactivate(t0.Add(35 * time.Minute))
	if len(late.Breaches) != 1 || !late.Breaches[0].At.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("deadline before the stop %+v", late.Breaches)
	}
	done := Start(snapshot(), 1, 1, t0, StartOptions{})
	done.Resolve(t0.Add(10 * time.Minute))
	done.Deactivate(t0.Add(20 * time.Minute))
	if !done.Current || done.ClosedReason != ClosedResolved || !done.ClosedAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("resolved cycle changed by the deactivation %+v", done)
	}
	met := Start(snapshot(), 1, 1, t0, StartOptions{})
	met.StaffPublicReply(t0.Add(5 * time.Minute))
	met.Deactivate(t0.Add(20 * time.Minute))
	if !met.Current || met.ClosedReason != ClosedMet {
		t.Fatalf("met cycle changed by the deactivation %+v", met)
	}
}
