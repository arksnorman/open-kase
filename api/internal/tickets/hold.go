package tickets

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// KindHoldExpire is the job that returns a ticket from On hold to Open when
// its hold ends (payload: ticketId, until as Unix milliseconds). It is
// enqueued in the transaction that puts the ticket on hold and removed in
// the transaction that ends the hold early, so it only ever fires for the
// hold it was scheduled for.
const KindHoldExpire = "ticket.holdExpire"

// HoldReasonPrefix starts the change entry that carries the hold reason name
// on the status event emitted when a hold ends (the email names the reason).
const HoldReasonPrefix = "holdReason:"

// HandleHoldExpire is the jobs.Handler for KindHoldExpire. The ticket
// returns to Open inside the job's fenced completion transaction, keeping
// its assignee; a ticket that is no longer on hold with this expiry (staff
// changed it, the requester replied) makes the job a no-op.
func (s *Service) HandleHoldExpire(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	ticketID := j.Payload["ticketId"]
	until, err := strconv.ParseInt(j.Payload["until"], 10, 64)
	if !validation.ValidID(j.ProjectId) || !validation.ValidID(ticketID) || err != nil {
		return jobs.Result{Outcome: jobs.Dead, ErrorClass: "invalidPayload", Error: "the hold expiry payload is invalid"}
	}
	return jobs.Result{Outcome: jobs.Done, Commit: func(ctx context.Context, f jobs.Finish) error {
		if f.State != jobs.StateSucceeded {
			return nil
		}
		if err := s.endHold(ctx, j.ProjectId, ticketID, until); err != nil && !errors.Is(err, ErrStale) {
			return err
		}
		return nil
	}}
}

// endHold moves a ticket whose hold expires at untilMs from On hold to Open
// as the automation actor.
func (s *Service) endHold(ctx context.Context, projectID, ticketID string, untilMs int64) error {
	t, err := s.AutomationTicket(ctx, projectID, ticketID)
	if err != nil {
		return err
	}
	if t.StatusCategory != statuses.CategoryOnHold || t.Hold == nil || t.Hold.Until.UnixMilli() != untilMs {
		return ErrStale
	}
	all, err := s.catalog.ProjectStatuses(ctx, t.WorkspaceId, t.ProjectId)
	if err != nil {
		return err
	}
	set, err := NewStatusSet(all)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	// The assignee stays only while still an eligible member of the ticket's group.
	eligible := false
	if t.Assignee != nil {
		if eligible, err = s.repo.IsEligible(ctx, t.ProjectId, t.Group.Id, t.Assignee.Id); err != nil {
			return err
		}
	}
	plan := Plan{Status: set.Open, ClearAssignee: t.Assignee != nil && !eligible}
	ch := Changes{UpdatedAt: now}
	changed := statusChanges(&ch, plan, t, now)
	reason := strings.TrimSpace(t.Hold.ReasonName)
	ev := audit.New(ctx, automationActor, audit.TicketHoldEnded, "ticket/"+t.Id, now)
	ev.ProjectId, ev.TicketId, ev.Changes, ev.Reason = t.ProjectId, t.Id, changed, "Hold period ended: "+reason
	sc := outbox.Event{Type: outbox.TicketStatusChanged, ProjectId: t.ProjectId, TicketId: t.Id, Visibility: VisibilityPublic,
		Actor: automationActor, OccurredAt: now, Transition: outbox.TransitionHoldEnded, Changes: []string{HoldReasonPrefix + reason}}
	m := Mutation{ProjectId: t.ProjectId, TicketId: t.Id, ExpectedVersion: t.Version,
		RequireCategories: []statuses.Category{statuses.CategoryOnHold}, Changes: ch,
		Audit: []audit.Event{ev}, Outbox: []outbox.Event{sc}}
	if t.Assignee != nil && eligible {
		m.Eligible = &Eligibility{GroupId: t.Group.Id, UserId: t.Assignee.Id}
	}
	_, _, err = s.repo.Mutate(ctx, m)
	if errors.Is(err, ErrPrecondition) || errors.Is(err, ErrNotFound) {
		return ErrStale
	}
	return err
}

// holdSweepGrace gives a live expiry job time to run before the sweep acts.
const holdSweepGrace = 10 * time.Minute

// SweepExpiredHolds ends holds that ran out while their expiry job was lost
// (dead-lettered after its attempts): the same endHold path, so the result
// is identical and a job that still runs later finds nothing to do. It
// returns how many tickets it reopened.
func (s *Service) SweepExpiredHolds(ctx context.Context, limit int) (int, error) {
	rows, err := s.repo.ExpiredHolds(ctx, s.clock.Now().Add(-holdSweepGrace), limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range rows {
		if t.Hold == nil {
			continue
		}
		switch err := s.endHold(ctx, t.ProjectId, t.Id, t.Hold.Until.UnixMilli()); {
		case err == nil:
			n++
		case errors.Is(err, ErrStale):
		default:
			return n, err
		}
	}
	return n, nil
}
