package automation

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// ScanInterval is how often an enabled project is scanned for due tickets.
const ScanInterval = 15 * time.Minute

// maxPerScan bounds the act jobs one scan enqueues per action.
const maxPerScan = 200

// Tickets is the ticket access automation needs (tickets.Service).
type Tickets interface {
	AutomationTicket(ctx context.Context, projectID, ticketID string) (tickets.Ticket, error)
	RemindRequester(ctx context.Context, t tickets.Ticket, gen int64, n int, autoResolveNotice bool) error
	AutoTransition(ctx context.Context, t tickets.Ticket, gen int64, action, reason string) error
}

// Deps wires the service.
type Deps struct {
	Repo    Repository
	Tickets Tickets
	Jobs    jobs.Store
	Waker   jobs.Waker
	Clock   clock.Clock
	Log     *slog.Logger
}

// Service manages policies and runs the automation jobs.
type Service struct{ d Deps }

// NewService returns the automation service.
func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{d: d}
}

// Get returns the project's policy (defaults when none was saved). Any
// signed-in project member may read it: the policy is visible to staff.
func (s *Service) Get(ctx context.Context, scope *access.ProjectScope) (Policy, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Policy{}, err
	}
	if err := scope.Require(access.CapTicketRead); err != nil {
		return Policy{}, err
	}
	return s.policy(ctx, scope.Principal.WorkspaceId, scope.Project.Id)
}

func (s *Service) policy(ctx context.Context, workspaceID, projectID string) (Policy, error) {
	p, err := s.d.Repo.GetPolicy(ctx, projectID)
	if errors.Is(err, ErrNotFound) {
		return Defaults(workspaceID, projectID), nil
	}
	return p, err
}

// Input replaces a policy.
type Input struct {
	ArchiveAfterResolvedDays int
	FollowupAfterHours       int
	MaxReminders             int
	ReminderCooldownHours    int
	AutoResolveAfterHours    int
}

func (in Input) validate() error {
	var v validation.Collector
	v.Int(validation.AutomationArchiveDays, "archiveAfterResolvedDays", int64(in.ArchiveAfterResolvedDays))
	v.Int(validation.AutomationFollowupHours, "customerFollowupAfterHours", int64(in.FollowupAfterHours))
	v.Int(validation.AutomationMaxReminders, "maxReminders", int64(in.MaxReminders))
	v.Int(validation.AutomationCooldownHours, "reminderCooldownHours", int64(in.ReminderCooldownHours))
	v.Int(validation.AutomationAutoResolveHrs, "autoResolveAfterHours", int64(in.AutoResolveAfterHours))
	if in.AutoResolveAfterHours > 0 && in.FollowupAfterHours == 0 {
		v.Add("autoResolveAfterHours", "requiresFollowup",
			"Auto-resolution requires follow-up reminders: the requester must be notified before a ticket is resolved.")
	}
	return v.Err()
}

// Put replaces the project's policy (admins). expectedVersion is
// the version read (0 when the project has no stored policy yet). An
// enabled policy starts scanning immediately.
func (s *Service) Put(ctx context.Context, scope *access.ProjectScope, expectedVersion int64, in Input) (Policy, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Policy{}, err
	}
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Policy{}, err
	}
	if err := in.validate(); err != nil {
		return Policy{}, err
	}
	cur, err := s.policy(ctx, scope.Principal.WorkspaceId, scope.Project.Id)
	if err != nil {
		return Policy{}, err
	}
	if cur.Version != expectedVersion {
		return Policy{}, fault.Conflict("versionConflict", "These automation settings were changed by someone else. Reload and try again.")
	}
	now := s.d.Clock.Now()
	p := Policy{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id,
		ArchiveAfterResolvedDays: in.ArchiveAfterResolvedDays, FollowupAfterHours: in.FollowupAfterHours,
		MaxReminders: in.MaxReminders, ReminderCooldownHours: in.ReminderCooldownHours, AutoResolveAfterHours: in.AutoResolveAfterHours,
		UpdatedAt: now, UpdatedBy: scope.AuditActor()}
	var changed []string
	for name, diff := range map[string]bool{
		"archiveAfterResolvedDays":   p.ArchiveAfterResolvedDays != cur.ArchiveAfterResolvedDays,
		"customerFollowupAfterHours": p.FollowupAfterHours != cur.FollowupAfterHours,
		"maxReminders":               p.MaxReminders != cur.MaxReminders,
		"reminderCooldownHours":      p.ReminderCooldownHours != cur.ReminderCooldownHours,
		"autoResolveAfterHours":      p.AutoResolveAfterHours != cur.AutoResolveAfterHours,
	} {
		if diff {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 && cur.Version > 0 {
		return cur, nil
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.AutomationUpdated, "automation/"+scope.Project.Id, now)
	ev.WorkspaceId, ev.ProjectId, ev.Changes = scope.Principal.WorkspaceId, scope.Project.Id, changed
	slices.Sort(ev.Changes)
	out, err := s.d.Repo.SavePolicy(ctx, p, expectedVersion, ev)
	if errors.Is(err, ErrVersionConflict) {
		return Policy{}, fault.Conflict("versionConflict", "These automation settings were changed by someone else. Reload and try again.")
	}
	if err != nil {
		return Policy{}, err
	}
	if out.Enabled() {
		if err := s.enqueueScan(ctx, out.WorkspaceId, out.ProjectId, now, "v"+strconv.FormatInt(out.Version, 10)); err != nil {
			s.d.Log.Warn("automation scan could not be scheduled; the periodic check will retry", "projectId", out.ProjectId, "error", err.Error())
		}
	}
	return out, nil
}

func (s *Service) enqueueScan(ctx context.Context, workspaceID, projectID string, at time.Time, tag string) error {
	_, err := s.d.Jobs.Enqueue(ctx, scanJob(workspaceID, projectID, at, tag))
	if err == nil && s.d.Waker != nil {
		s.d.Waker.Notify(ctx)
	}
	return err
}

func scanJob(workspaceID, projectID string, at time.Time, tag string) jobs.NewJob {
	return jobs.NewJob{WorkspaceId: workspaceID, ProjectId: projectID, Kind: KindScan,
		Key: "automation:scan:" + projectID + ":" + tag, Payload: map[string]string{"projectId": projectID}, RunAt: at, MaxAttempts: 3}
}

// windowTag names the scan window containing at. Periodic scans are keyed
// by their window, so a project has at most one periodic scan per window no
// matter who schedules it (a finished scan, the maintenance loop of any
// process): every scan schedules a successor, and successors with distinct
// keys would each start another perpetual chain.
func windowTag(at time.Time) string {
	return "w" + strconv.FormatInt(at.Truncate(ScanInterval).Unix(), 10)
}

// EnsureScheduled enqueues a scan for every project with an enabled
// policy, keyed by the current scan window so repeated calls (every process,
// every cleanup tick) create at most one scan per window. It keeps
// automation running even if a scan chain was dead-lettered.
func (s *Service) EnsureScheduled(ctx context.Context) (int, error) {
	refs, err := s.d.Repo.EnabledProjects(ctx)
	if err != nil {
		return 0, err
	}
	now := s.d.Clock.Now()
	n := 0
	for _, r := range refs {
		c, err := s.d.Jobs.Enqueue(ctx, scanJob(r.WorkspaceId, r.ProjectId, now, windowTag(now)))
		if err != nil {
			return n, err
		}
		n += c
	}
	if n > 0 && s.d.Waker != nil {
		s.d.Waker.Notify(ctx)
	}
	return n, nil
}

// HandleScan is the jobs.Handler for KindScan: it enqueues act jobs for
// the project's due tickets and, while the policy stays enabled, the next
// scan (in the fenced completion transaction).
func (s *Service) HandleScan(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	projectID := j.Payload["projectId"]
	if !validation.ValidID(projectID) {
		return jobs.Result{Outcome: jobs.Dead, ErrorClass: "invalidPayload", Error: "the automation scan payload is invalid"}
	}
	p, err := s.policy(ctx, j.WorkspaceId, projectID)
	if err != nil {
		return jobs.Result{Outcome: jobs.Retry, ErrorClass: "database", Error: err.Error()}
	}
	if !p.Enabled() {
		return jobs.Result{Outcome: jobs.Done}
	}
	now := s.d.Clock.Now()
	cands, err := s.d.Repo.Candidates(ctx, p, now, maxPerScan)
	if err != nil {
		return jobs.Result{Outcome: jobs.Retry, ErrorClass: "database", Error: err.Error()}
	}
	acts := make([]jobs.NewJob, 0, len(cands))
	window := windowTag(now)
	for _, c := range cands {
		gen := strconv.FormatInt(c.Generation, 10)
		n := strconv.Itoa(c.Reminders)
		// Keyed by the scan window too: an action that turned out to be a
		// no-op (policy switched off meanwhile, ticket changed)
		// must not block the same action for the rest of the waiting period
		// (pass-2 review OC-ASYNC-08). A second action in a later window is
		// harmless: act re-checks generation and reminder count inside the
		// fenced transaction, so the action is performed at most once.
		acts = append(acts, jobs.NewJob{WorkspaceId: j.WorkspaceId, ProjectId: projectID, Kind: KindAct,
			Key:     "automation:" + c.TicketId + ":" + gen + ":" + c.Action + ":" + n + ":" + window,
			Payload: map[string]string{"projectId": projectID, "ticketId": c.TicketId, "generation": gen, "action": c.Action, "reminders": n},
			RunAt:   now, MaxAttempts: 3})
	}
	if len(acts) > 0 {
		if _, err := s.d.Jobs.Enqueue(ctx, acts...); err != nil {
			return jobs.Result{Outcome: jobs.Retry, ErrorClass: "database", Error: err.Error()}
		}
		if s.d.Waker != nil {
			s.d.Waker.Notify(ctx)
		}
	}
	// The successor is keyed by its window (not by its exact time): scans
	// seeded by a policy save or by the maintenance loop then join the one
	// chain instead of each starting its own.
	next := now.Add(ScanInterval)
	return jobs.Result{Outcome: jobs.Done, Commit: func(ctx context.Context, f jobs.Finish) error {
		if f.State != jobs.StateSucceeded {
			return nil
		}
		_, err := s.d.Jobs.Enqueue(ctx, scanJob(j.WorkspaceId, projectID, next, windowTag(next)))
		return err
	}}
}

// HandleAct is the jobs.Handler for KindAct. The action runs inside the
// job's fenced completion transaction after re-reading the ticket and the
// policy: a stale generation, another status, a changed reminder count, a
// policy that no longer makes the action due, or a missing/archived ticket
// make it a no-op. A worker whose lease was taken over commits nothing.
func (s *Service) HandleAct(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	projectID, ticketID, action := j.Payload["projectId"], j.Payload["ticketId"], j.Payload["action"]
	gen, gerr := strconv.ParseInt(j.Payload["generation"], 10, 64)
	n, nerr := strconv.Atoi(j.Payload["reminders"])
	if !validation.ValidID(projectID) || !validation.ValidID(ticketID) || gerr != nil || nerr != nil ||
		(action != ActionRemind && action != ActionResolve && action != ActionArchive) {
		return jobs.Result{Outcome: jobs.Dead, ErrorClass: "invalidPayload", Error: "the automation payload is invalid"}
	}
	return jobs.Result{Outcome: jobs.Done, Commit: func(ctx context.Context, f jobs.Finish) error {
		if f.State != jobs.StateSucceeded {
			return nil
		}
		err := s.act(ctx, j.WorkspaceId, projectID, ticketID, gen, n, action)
		if errors.Is(err, tickets.ErrStale) {
			s.d.Log.Debug("automation action skipped: ticket changed", "ticketId", ticketID, "action", action, "generation", gen)
			return nil
		}
		return err
	}}
}

// Act performs one automation action if it is still due (inside the
// caller's transaction). It returns tickets.ErrStale when nothing was done.
func (s *Service) act(ctx context.Context, workspaceID, projectID, ticketID string, gen int64, n int, action string) error {
	p, err := s.policy(ctx, workspaceID, projectID)
	if err != nil {
		return err
	}
	t, err := s.d.Tickets.AutomationTicket(ctx, projectID, ticketID)
	if err != nil {
		return err
	}
	if t.Automation.Generation != gen || t.Automation.Reminders != n {
		return tickets.ErrStale
	}
	due, ok := Due(p, t, s.d.Clock.Now())
	if !ok || due != action {
		return tickets.ErrStale
	}
	switch action {
	case ActionRemind:
		notice := p.AutoResolves() && n+1 >= p.MaxReminders
		return s.d.Tickets.RemindRequester(ctx, t, gen, n, notice)
	case ActionResolve:
		return s.d.Tickets.AutoTransition(ctx, t, gen, "resolve",
			"Automatically resolved: no reply from the requester "+strconv.Itoa(p.AutoResolveAfterHours)+" hours after the last reminder (lifecycle automation).")
	default:
		return s.d.Tickets.AutoTransition(ctx, t, gen, "archive",
			"Automatically archived "+strconv.Itoa(p.ArchiveAfterResolvedDays)+" days after resolution (lifecycle automation).")
	}
}
