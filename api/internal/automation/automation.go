// Package automation implements per-project lifecycle automation
// (architecture §3.4): archiving tickets resolved for a configured number of
// days, follow-up reminders to the requester of tickets waiting for them,
// and optional auto-resolution after the reminders went unanswered.
//
// Everything is driven by durable jobs (Milestone 5 framework). A periodic
// automation.scan job per project finds due tickets and enqueues one
// automation.act job per ticket, action and automation generation. The act
// job re-reads the ticket and the policy inside its fenced completion
// transaction and acts only when the ticket still has the job's generation
// and the policy still makes the action due. Every status change and every
// public reply moves the ticket's automation generation in the same
// transaction as the change, so a requester reply cancels pending
// reminders and auto-resolution atomically. Archived tickets
// are never targeted. No in-memory timers are involved.
package automation

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// Job kinds.
const (
	KindScan = "automation.scan"
	KindAct  = "automation.act"
)

// Actions.
const (
	ActionRemind  = "remind"
	ActionResolve = "resolve"
	ActionArchive = "archive"
)

// Policy is one project's lifecycle automation. Zero durations disable the
// corresponding rule; everything is disabled by default.
type Policy struct {
	WorkspaceId string
	ProjectId   string
	// ArchiveAfterResolvedDays archives tickets resolved this many days
	// ago (0 = disabled).
	ArchiveAfterResolvedDays int
	// FollowupAfterHours sends the requester a reminder when a ticket has
	// waited this long for them (0 = disabled).
	FollowupAfterHours int
	// MaxReminders bounds reminders per waiting period; ReminderCooldown
	// Hours separates consecutive reminders.
	MaxReminders          int
	ReminderCooldownHours int
	// AutoResolveAfterHours resolves a waiting ticket this long after the
	// last reminder (0 = disabled). It requires reminders: the requester
	// always receives prior notice.
	AutoResolveAfterHours int
	Version               int64
	UpdatedAt             time.Time
	UpdatedBy             audit.Actor
}

// Defaults is the policy of a project that never saved one (disabled).
func Defaults(workspaceID, projectID string) Policy {
	return Policy{WorkspaceId: workspaceID, ProjectId: projectID, MaxReminders: 2, ReminderCooldownHours: 24}
}

// Enabled reports whether any rule is active.
func (p Policy) Enabled() bool {
	return p.ArchiveAfterResolvedDays > 0 || p.FollowupAfterHours > 0
}

// AutoResolves reports whether waiting tickets are resolved after the
// reminders.
func (p Policy) AutoResolves() bool { return p.FollowupAfterHours > 0 && p.AutoResolveAfterHours > 0 }

func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

// Due returns the action the policy makes due for t at now. Archived
// tickets never have one.
func Due(p Policy, t tickets.Ticket, now time.Time) (string, bool) {
	if t.Archived() {
		return "", false
	}
	a := t.Automation
	switch t.StatusCategory {
	case statuses.CategoryResolved:
		if p.ArchiveAfterResolvedDays > 0 && t.ResolvedAt != nil &&
			!t.ResolvedAt.Add(time.Duration(p.ArchiveAfterResolvedDays)*24*time.Hour).After(now) {
			return ActionArchive, true
		}
	case statuses.CategoryWaitingCustomer:
		if p.FollowupAfterHours <= 0 || a.WaitingSince == nil {
			return "", false
		}
		if a.Reminders < p.MaxReminders {
			at := a.WaitingSince.Add(hours(p.FollowupAfterHours))
			if a.Reminders > 0 && a.LastReminderAt != nil {
				at = a.LastReminderAt.Add(hours(p.ReminderCooldownHours))
			}
			if !at.After(now) {
				return ActionRemind, true
			}
			return "", false
		}
		if !p.AutoResolves() || a.LastReminderAt == nil {
			return "", false
		}
		if !a.NoticeGiven {
			// The reminders went out without the notice (auto-resolution
			// was enabled, or the reminder count lowered, afterwards): one
			// final reminder carries it before anything is resolved.
			if !a.LastReminderAt.Add(hours(p.ReminderCooldownHours)).After(now) {
				return ActionRemind, true
			}
			return "", false
		}
		if !a.LastReminderAt.Add(hours(p.AutoResolveAfterHours)).After(now) {
			return ActionResolve, true
		}
	}
	return "", false
}

// Candidate is a ticket a scan found due for an action.
type Candidate struct {
	TicketId   string
	Generation int64
	Reminders  int
	Action     string
}

// ProjectRef identifies a project with an enabled policy.
type ProjectRef struct {
	WorkspaceId string
	ProjectId   string
}

// ErrNotFound is returned when a project has no stored policy.
var ErrNotFound = errors.New("automation: policy not found")

// ErrVersionConflict is returned when the stored policy changed.
var ErrVersionConflict = errors.New("automation: version conflict")

// Repository persists policies and finds due tickets.
type Repository interface {
	GetPolicy(ctx context.Context, projectID string) (Policy, error)
	// SavePolicy upserts the policy when its version still equals
	// expectedVersion (0 for a project without one) and records ev.
	SavePolicy(ctx context.Context, p Policy, expectedVersion int64, ev audit.Event) (Policy, error)
	// Candidates returns live tickets of the project for which the policy
	// may make an action due at now (rechecked with Due before acting).
	Candidates(ctx context.Context, p Policy, now time.Time, limit int) ([]Candidate, error)
	// EnabledProjects lists projects whose policy has a rule enabled.
	EnabledProjects(ctx context.Context) ([]ProjectRef, error)
}
