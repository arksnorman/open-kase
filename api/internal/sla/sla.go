// Package sla computes the first-response service-level target of tickets
// per priority, in wall-clock time (24/7, no working hours), with a snapshot
// of the SLA version stored on every ticket cycle.
//
// The engine in this file is pure: every operation takes the current time
// and changes an Instance value; persistence, jobs and notifications are
// the callers' concern. Deadlines are stored in UTC.
//
// Rules implemented here:
//   - A ticket has one cycle with one clock, first response. It starts
//     when the ticket is created; the first public reply by staff (a user
//     or a token posting as staff) satisfies it and closes the cycle as met. Agent
//     notes, assignment and requester replies never do (callers only
//     report staff public replies).
//   - Resolving or archiving before any staff reply stops the clock (not
//     met) and closes the cycle. Reopening starts nothing: a reopened
//     ticket gets no new SLA.
//   - A priority change keeps the elapsed time and recalculates the
//     remaining budget from the cycle's own snapshot; breaches that
//     already happened are never erased.
//   - Turning SLA off stops open cycles (no longer current, so the ticket
//     shows no SLA); breaches recorded before stay. Turning it on again
//     starts nothing by itself (new tickets, the demo seed's backfill).
package sla

import (
	"slices"
	"time"
)

// Kind is a target kind.
type Kind string

// FirstResponse is the only target kind.
const FirstResponse Kind = "firstResponse"

// State is a clock's state.
type State string

// Clock states.
const (
	Running State = "running" // counting towards the deadline
	Met     State = "met"     // satisfied
	Stopped State = "stopped" // ended without being satisfied (resolved or archived first)
)

// Reasons a cycle was closed (Instance.ClosedReason).
const (
	ClosedMet         = "met"      // a staff member replied in public
	ClosedResolved    = "resolved" // the ticket was resolved before a staff reply
	ClosedArchived    = "archived" // the ticket was archived before a staff reply
	ClosedDeactivated = "slaOff"   // SLA was turned off
)

// Targets are the budgets for one priority: FirstResponseMinutes is the
// first-response budget in wall-clock minutes, 24/7 (0 = no target).
type Targets struct {
	FirstResponseMinutes int
}

// Empty reports whether the priority has no target.
func (t Targets) Empty() bool { return t.FirstResponseMinutes == 0 }

// Snapshot is the SLA version a cycle was started with.
type Snapshot struct {
	PolicyId       string
	PolicyVersion  int64
	Targets        [4]Targets // index = priority - 1
	WarningPercent int        // due soon once this share of the budget is used
}

func (s Snapshot) targets(priority int) Targets {
	if priority < 1 || priority > 4 {
		return Targets{}
	}
	return s.Targets[priority-1]
}

// Clock measures one target.
type Clock struct {
	Kind          Kind
	State         State
	BudgetSeconds int64 // 0 = no target for the current priority
	// ElapsedSeconds is the measured time accrued before RunningSince.
	ElapsedSeconds   int64
	RunningSince     *time.Time
	StartedAt        *time.Time // start of the current obligation
	DueAt            *time.Time // running with a budget only
	WarnAt           *time.Time
	MetAt            *time.Time
	StoppedAt        *time.Time
	BreachedAt       *time.Time // when the deadline passed (never erased)
	WarnedAt         *time.Time // a warning event was emitted
	BreachNotifiedAt *time.Time // a breach event was emitted
}

// Breach records one missed deadline (bounded history per cycle).
type Breach struct {
	Kind Kind
	At   time.Time
}

// MaxBreaches bounds the breach history of one cycle.
const MaxBreaches = 50

// Instance is one SLA cycle of a ticket.
type Instance struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	TicketId    string
	Cycle       int
	// Current is true for the ticket's latest cycle.
	Current  bool
	Snapshot Snapshot
	Priority int
	Clocks   []Clock
	Breaches []Breach
	// Generation changes whenever the schedule (deadlines, warnings or
	// notification state) changes; timer jobs carry it and do nothing when
	// it is stale.
	Generation int64
	StartedAt  time.Time
	ClosedAt   *time.Time
	// ClosedReason says why the cycle closed (Closed* constants).
	ClosedReason string
	// Quiet cycles (demo data) record events without notifying anyone.
	Quiet bool
	// Rev increments on every write (optimistic concurrency).
	Rev       int64
	UpdatedAt time.Time
}

// StartOptions shape a new cycle.
type StartOptions struct {
	Quiet bool
}

func tp(t time.Time) *time.Time { return &t }

// Start begins a cycle at now for the given priority: the first-response
// clock runs.
func Start(snap Snapshot, priority, cycle int, now time.Time, o StartOptions) Instance {
	in := Instance{Cycle: cycle, Current: true, Snapshot: snap, Priority: priority, StartedAt: now, Quiet: o.Quiet, Generation: 1, UpdatedAt: now}
	tg := snap.targets(priority)
	in.Clocks = []Clock{{Kind: FirstResponse, State: Running, BudgetSeconds: int64(tg.FirstResponseMinutes) * 60,
		RunningSince: tp(now), StartedAt: tp(now)}}
	in.recompute()
	return in
}

// Clone returns a deep copy.
func (in Instance) Clone() Instance {
	out := in
	out.Clocks = slices.Clone(in.Clocks)
	out.Breaches = slices.Clone(in.Breaches)
	return out
}

// Clock returns the clock of kind k, or nil.
func (in *Instance) Clock(k Kind) *Clock {
	for i := range in.Clocks {
		if in.Clocks[i].Kind == k {
			return &in.Clocks[i]
		}
	}
	return nil
}

// Closed reports whether the cycle ended (met, resolved, archived, stopped).
func (in Instance) Closed() bool { return in.ClosedAt != nil }

// measure returns the wall-clock seconds between from and to.
func measure(from, to time.Time) int64 {
	if !to.After(from) {
		return 0
	}
	return int64(to.Sub(from) / time.Second)
}

// deadline returns from plus seconds (at least from).
func deadline(from time.Time, seconds int64) *time.Time {
	return tp(from.Add(time.Duration(max(seconds, 0)) * time.Second))
}

// accrue moves running time into ElapsedSeconds and restarts the running
// interval at now.
func (in *Instance) accrue(c *Clock, now time.Time) {
	if c.State != Running || c.RunningSince == nil {
		return
	}
	c.ElapsedSeconds += measure(*c.RunningSince, now)
	c.RunningSince = tp(now)
}

// Advance records deadlines that passed by now. It is called before every
// event so breaches keep the time at which they happened.
func (in *Instance) Advance(now time.Time) {
	for i := range in.Clocks {
		c := &in.Clocks[i]
		if c.State != Running || c.DueAt == nil || c.BreachedAt != nil || c.DueAt.After(now) {
			continue
		}
		c.BreachedAt = tp(*c.DueAt)
		in.Breaches = append(in.Breaches, Breach{Kind: c.Kind, At: *c.DueAt})
		if len(in.Breaches) > MaxBreaches {
			in.Breaches = in.Breaches[len(in.Breaches)-MaxBreaches:]
		}
	}
}

func (in *Instance) warningPercent() int64 {
	p := int64(in.Snapshot.WarningPercent)
	if p <= 0 || p >= 100 {
		return 80
	}
	return p
}

// recompute derives the deadlines of running clocks and bumps the
// generation when the schedule changed; every operation calls it.
func (in *Instance) recompute() {
	before := in.signature()
	for i := range in.Clocks {
		c := &in.Clocks[i]
		c.DueAt, c.WarnAt = nil, nil
		if c.State != Running || c.BudgetSeconds <= 0 || c.RunningSince == nil {
			continue
		}
		since := *c.RunningSince
		c.DueAt = deadline(since, c.BudgetSeconds-c.ElapsedSeconds)
		c.WarnAt = deadline(since, c.BudgetSeconds*in.warningPercent()/100-c.ElapsedSeconds)
	}
	if in.signature() != before {
		in.Generation++
	}
}

func (in *Instance) signature() string {
	s := ""
	for _, c := range in.Clocks {
		s += string(c.Kind) + string(c.State) + fmtTime(c.DueAt) + fmtTime(c.WarnAt) + fmtTime(c.WarnedAt) + fmtTime(c.BreachNotifiedAt) + "|"
	}
	return s
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (in *Instance) touch(now time.Time) {
	in.recompute()
	in.UpdatedAt = now
}

func (in *Instance) satisfy(c *Clock, now time.Time) {
	if c == nil || c.State != Running {
		return
	}
	in.accrue(c, now)
	c.State, c.MetAt, c.RunningSince = Met, tp(now), nil
}

func (in *Instance) stop(c *Clock, now time.Time) {
	if c == nil || c.State != Running {
		return
	}
	in.accrue(c, now)
	c.State, c.StoppedAt, c.RunningSince = Stopped, tp(now), nil
}

// StaffPublicReply satisfies the first response and closes the cycle as
// met. Only a public message by staff counts; callers never report agent
// notes or requester replies. A closed cycle is left as it is.
func (in *Instance) StaffPublicReply(now time.Time) {
	if in.Closed() {
		return
	}
	in.Advance(now)
	for i := range in.Clocks {
		in.satisfy(&in.Clocks[i], now)
	}
	in.ClosedAt, in.ClosedReason = tp(now), ClosedMet
	in.touch(now)
}

// Resolve closes an open cycle when the ticket is resolved before any staff
// reply: the first response stops without being met. The cycle stays
// current; reopening the ticket starts no new one.
func (in *Instance) Resolve(now time.Time) { in.finish(now, ClosedResolved) }

// Archive is Resolve for a ticket archived before any staff reply.
func (in *Instance) Archive(now time.Time) { in.finish(now, ClosedArchived) }

func (in *Instance) finish(now time.Time, reason string) {
	if in.Closed() {
		return
	}
	in.closeClocks(now)
	in.ClosedAt, in.ClosedReason = tp(now), reason
	in.touch(now)
}

// closeClocks records deadlines passed by now and stops every open clock.
func (in *Instance) closeClocks(now time.Time) {
	in.Advance(now)
	for i := range in.Clocks {
		in.stop(&in.Clocks[i], now)
	}
}

// ChangePriority keeps elapsed time and recalculates every open clock's
// budget from the cycle's snapshot. A budget smaller than the elapsed time
// breaches at once; earlier breaches stay recorded.
func (in *Instance) ChangePriority(priority int, now time.Time) {
	if in.Closed() {
		return
	}
	in.Advance(now)
	in.Priority = priority
	tg := in.Snapshot.targets(priority)
	for i := range in.Clocks {
		c := &in.Clocks[i]
		if c.State == Met || c.State == Stopped {
			continue
		}
		in.accrue(c, now)
		c.BudgetSeconds = int64(tg.FirstResponseMinutes) * 60
	}
	in.touch(now)
	in.Advance(now)
}

// Deactivate ends an open cycle because SLA was turned off at the given
// time: deadlines that passed by then are recorded (breaches
// recorded earlier stay), every clock stops and the cycle is no longer
// current, so the ticket shows no SLA until a new cycle starts. A closed
// cycle (met, resolved, archived) is left as it is.
func (in *Instance) Deactivate(at time.Time) {
	if in.Closed() {
		return
	}
	in.Current = false
	in.closeClocks(at)
	in.ClosedAt, in.ClosedReason = tp(at), ClosedDeactivated
	in.touch(at)
}

// Notice is an SLA event to emit.
type Notice struct {
	Kind     Kind
	Breached bool // false: warning (due soon)
	DueAt    time.Time
}

// Check records breaches and returns the warnings and breaches that have
// not been announced yet, marking them announced.
func (in *Instance) Check(now time.Time) []Notice {
	if in.Closed() {
		return nil
	}
	in.Advance(now)
	var out []Notice
	for i := range in.Clocks {
		c := &in.Clocks[i]
		if c.State != Running || c.BudgetSeconds <= 0 || c.DueAt == nil {
			continue
		}
		switch {
		case c.BreachedAt != nil && c.BreachNotifiedAt == nil:
			c.BreachNotifiedAt = tp(now)
			if c.WarnedAt == nil {
				c.WarnedAt = tp(now) // no separate warning after the deadline
			}
			out = append(out, Notice{Kind: c.Kind, Breached: true, DueAt: *c.DueAt})
		case c.BreachedAt == nil && c.WarnedAt == nil && c.WarnAt != nil && !c.WarnAt.After(now):
			c.WarnedAt = tp(now)
			out = append(out, Notice{Kind: c.Kind, DueAt: *c.DueAt})
		}
	}
	in.touch(now)
	return out
}

// NextCheck returns when the next warning or breach is due to be announced,
// or nil when nothing is pending.
func (in Instance) NextCheck() *time.Time {
	if in.Closed() {
		return nil
	}
	var next *time.Time
	consider := func(t *time.Time) {
		if t != nil && (next == nil || t.Before(*next)) {
			next = tp(*t)
		}
	}
	for _, c := range in.Clocks {
		if c.State != Running || c.BudgetSeconds <= 0 {
			continue
		}
		if c.WarnedAt == nil && c.BreachedAt == nil {
			consider(c.WarnAt)
		}
		if c.BreachNotifiedAt == nil {
			consider(c.DueAt)
		}
	}
	return next
}

// Projection returns the earliest running deadline and warning time, used
// to index tickets for the due-soon views.
func (in Instance) Projection() (due, warn *time.Time) {
	if in.Closed() || !in.Current {
		return nil, nil
	}
	for _, c := range in.Clocks {
		if c.State != Running || c.DueAt == nil {
			continue
		}
		if due == nil || c.DueAt.Before(*due) {
			due = tp(*c.DueAt)
		}
		if c.WarnAt != nil && (warn == nil || c.WarnAt.Before(*warn)) {
			warn = tp(*c.WarnAt)
		}
	}
	return due, warn
}

// Outcome reports how a current cycle's first response went: "met" (in
// time), "missed" (answered after the deadline) or "" (still running,
// stopped, or not current).
func (in Instance) Outcome() string {
	if !in.Current {
		return ""
	}
	for _, c := range in.Clocks {
		if c.State == Met && c.BudgetSeconds > 0 {
			if c.BreachedAt != nil {
				return "missed"
			}
			return "met"
		}
	}
	return ""
}

// Summary states.
const (
	SummaryOK       = "ok"
	SummaryDueSoon  = "dueSoon"
	SummaryBreached = "breached"
	SummaryMet      = "met"
)

// ClockView is a clock as seen at a point in time.
type ClockView struct {
	Kind           Kind
	State          State
	BudgetSeconds  int64
	ElapsedSeconds int64
	DueAt          *time.Time
	WarnAt         *time.Time
	MetAt          *time.Time
	BreachedAt     *time.Time
}

// Summary is a ticket's SLA status at a point in time.
type Summary struct {
	Kind  Kind
	State string
	DueAt *time.Time
	// Breached reports whether any target of the cycle missed its deadline.
	Breached bool
	Cycle    int
	// Version is the SLA version the cycle started with.
	Version int64
	Clocks  []ClockView
}

// Summarize returns the ticket-level SLA view at now, or nil when the
// cycle has no target for its priority.
func (in Instance) Summarize(now time.Time) *Summary {
	s := &Summary{Cycle: in.Cycle, Version: in.Snapshot.PolicyVersion, Breached: len(in.Breaches) > 0}
	any := false
	var running, met *Clock
	for i := range in.Clocks {
		c := &in.Clocks[i]
		v := ClockView{Kind: c.Kind, State: c.State, BudgetSeconds: c.BudgetSeconds,
			ElapsedSeconds: c.ElapsedSeconds, DueAt: c.DueAt, WarnAt: c.WarnAt, MetAt: c.MetAt, BreachedAt: c.BreachedAt}
		if c.State == Running && c.RunningSince != nil {
			v.ElapsedSeconds += measure(*c.RunningSince, now)
		}
		if c.State == Running && c.DueAt != nil && c.BreachedAt == nil && !c.DueAt.After(now) {
			v.BreachedAt = tp(*c.DueAt)
		}
		if v.BreachedAt != nil {
			s.Breached = true
		}
		s.Clocks = append(s.Clocks, v)
		if c.BudgetSeconds <= 0 {
			continue
		}
		any = true
		switch c.State {
		case Running:
			if c.DueAt != nil && (running == nil || running.DueAt == nil || c.DueAt.Before(*running.DueAt)) {
				running = c
			}
		case Met:
			if met == nil || (c.MetAt != nil && met.MetAt != nil && c.MetAt.After(*met.MetAt)) {
				met = c
			}
		}
	}
	if !any {
		return nil
	}
	switch {
	case running != nil:
		s.Kind, s.DueAt = running.Kind, tp(*running.DueAt)
		switch {
		case running.BreachedAt != nil || !running.DueAt.After(now):
			s.State = SummaryBreached
		case running.WarnAt != nil && !running.WarnAt.After(now):
			s.State = SummaryDueSoon
		default:
			s.State = SummaryOK
		}
	case met != nil:
		s.Kind, s.State = met.Kind, SummaryMet
	default:
		return nil // only stopped clocks: no current obligation to show
	}
	return s
}
