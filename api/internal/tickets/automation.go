package tickets

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/statuses"
)

// OutboxFollowupReminder asks the requester for a reply: an email to the
// requester contact (skipped when requester emails are off) and a
// ticket.followupReminder webhook; no staff notification.
const OutboxFollowupReminder = outbox.TicketFollowupReminder

// ReminderMarkAutoResolve is recorded in a reminder event's changes when
// the project will auto-resolve the ticket if the requester stays silent.
const ReminderMarkAutoResolve = "autoResolveNotice"

// ReminderMarkNumber prefixes the reminder's number (1-based, within the
// current waiting period) in a reminder event's changes: "reminder:2".
const ReminderMarkNumber = "reminder:"

// automationActor is the audit/outbox actor of lifecycle automation.
var automationActor = audit.System(audit.SystemAutomation, "lifecycle automation")

// ReminderCommand records one follow-up reminder.
type ReminderCommand struct {
	ProjectId  string
	TicketId   string
	Generation int64
	// Reminders is the count before this reminder (the stored count must
	// still equal it).
	Reminders int
	// Notice records that this reminder announces the automatic resolution.
	Notice bool
	At     time.Time
	Audit  []audit.Event
	Outbox []outbox.Event
}

// ErrStale means an automation job's ticket state moved on (another
// generation, status or reminder count).
var ErrStale = errors.New("tickets: automation target changed")

// AutomationTicket reads a live ticket for an automation job (no access
// scope: the job acts as the system under the project's policy).
func (s *Service) AutomationTicket(ctx context.Context, projectID, ticketID string) (Ticket, error) {
	t, err := s.repo.Get(ctx, projectID, ticketID, "")
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, ErrStale
	}
	return t, err
}

// RemindRequester records a follow-up reminder for a ticket that is still
// waiting for the requester at generation gen with n reminders sent. The
// requester receives an email through the outbox (template
// followupReminder). The ticket version is not changed.
func (s *Service) RemindRequester(ctx context.Context, t Ticket, gen int64, n int, autoResolveNotice bool) error {
	now := s.clock.Now()
	ev := audit.New(ctx, automationActor, audit.TicketFollowupReminderSent, "ticket/"+t.Id, now)
	ev.ProjectId, ev.TicketId, ev.Changes = t.ProjectId, t.Id, []string{"automation.reminders"}
	ob := outbox.Event{Type: OutboxFollowupReminder, ProjectId: t.ProjectId, TicketId: t.Id, Visibility: VisibilityPublic,
		Actor: automationActor, OccurredAt: now}
	ob.Changes = []string{ReminderMarkNumber + strconv.Itoa(n+1)}
	if autoResolveNotice {
		ob.Changes = append(ob.Changes, ReminderMarkAutoResolve)
	}
	ok, err := s.repo.RemindRequester(ctx, ReminderCommand{ProjectId: t.ProjectId, TicketId: t.Id, Generation: gen, Reminders: n,
		Notice: autoResolveNotice, At: now, Audit: []audit.Event{ev}, Outbox: []outbox.Event{ob}})
	if err != nil {
		return err
	}
	if !ok {
		return ErrStale
	}
	return nil
}

// AutoTransition resolves a ticket that stayed waiting for the requester
// (action "resolve") or archives a ticket resolved long enough (action
// "archive") on behalf of the project's automation policy. It only applies
// while the ticket still has automation generation gen, the expected
// status category; otherwise it returns ErrStale.
func (s *Service) AutoTransition(ctx context.Context, t Ticket, gen int64, action, reason string) error {
	all, err := s.catalog.ProjectStatuses(ctx, t.WorkspaceId, t.ProjectId)
	if err != nil {
		return err
	}
	set, err := NewStatusSet(all)
	if err != nil {
		return err
	}
	var require []statuses.Category
	switch action {
	case "resolve":
		require = []statuses.Category{statuses.CategoryWaitingCustomer}
	case "archive":
		require = []statuses.Category{statuses.CategoryResolved}
	default:
		return errors.New("tickets: unknown automation action " + action)
	}
	if t.Automation.Generation != gen || t.StatusCategory != require[0] {
		return ErrStale
	}
	plan, err := PlanTransition(t, action, nil, set, false)
	if err != nil {
		return ErrStale
	}
	now := s.clock.Now()
	ch := Changes{UpdatedAt: now}
	changed := statusChanges(&ch, plan, t, now)
	m := Mutation{ProjectId: t.ProjectId, TicketId: t.Id, ExpectedVersion: t.Version, RequireCategories: require, Changes: ch,
		RequireAutomationGen: &gen}
	actionName := audit.TicketResolved
	if action == "resolve" {
		m.SLA = slaFor(func(c *sla.Instance) { c.Resolve(now) })
	} else {
		actionName = audit.TicketArchived
		m.SLA = slaFor(func(c *sla.Instance) { c.Archive(now) })
	}
	ev := audit.New(ctx, automationActor, actionName, "ticket/"+t.Id, now)
	ev.ProjectId, ev.TicketId, ev.Changes, ev.Reason = t.ProjectId, t.Id, changed, reason
	m.Audit = []audit.Event{ev}
	sc := outbox.Event{Type: outbox.TicketStatusChanged, ProjectId: t.ProjectId, TicketId: t.Id, Visibility: VisibilityPublic,
		Actor: automationActor, OccurredAt: now, Transition: action}
	m.Outbox = []outbox.Event{sc}
	_, _, err = s.repo.Mutate(ctx, m)
	if errors.Is(err, ErrPrecondition) || errors.Is(err, ErrNotFound) {
		return ErrStale
	}
	return err
}
