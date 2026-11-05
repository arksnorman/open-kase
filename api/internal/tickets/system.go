package tickets

import (
	"context"
	"errors"
	"slices"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/package/fault"
)

// GetMessage returns one message of an accessible ticket when the scope may
// read the message's audience. Anything else is 404 messageNotFound, so
// an agent note is indistinguishable from a missing message for
// integrations.
func (s *Service) GetMessage(ctx context.Context, scope *access.ProjectScope, ticketRef, messageID string) (Ticket, Message, error) {
	audiences, err := readableAudiences(scope)
	if err != nil {
		return Ticket{}, Message{}, err
	}
	t, err := s.load(ctx, scope, ticketRef)
	if err != nil {
		return Ticket{}, Message{}, err
	}
	m, err := s.repo.GetMessage(ctx, t.Id, messageID)
	if errors.Is(err, ErrNotFound) || (err == nil && !slices.Contains(audiences, m.Visibility)) {
		return Ticket{}, Message{}, errMessageNotFound()
	}
	if err != nil {
		return Ticket{}, Message{}, err
	}
	return s.present(scope, t), m, nil
}

func errMessageNotFound() error { return fault.NotFound("messageNotFound") }

// SystemTicket reads a ticket without an access scope, for workers that
// then authorize each recipient separately.
func (s *Service) SystemTicket(ctx context.Context, projectID, ticketID string) (Ticket, error) {
	t, err := s.repo.Get(ctx, projectID, ticketID, "")
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, errTicketNotFound()
	}
	return t, err
}

// SystemMessage reads a message without an access scope (workers).
func (s *Service) SystemMessage(ctx context.Context, ticketID, messageID string) (Message, error) {
	m, err := s.repo.GetMessage(ctx, ticketID, messageID)
	if errors.Is(err, ErrNotFound) {
		return Message{}, errMessageNotFound()
	}
	return m, err
}

// SystemReleaseReason is the release reason recorded when the system
// returns a ticket to its queue because the assignee lost eligibility.
const SystemReleaseReason = "Automatic release: the assignee is no longer an active member of the group (disabled, or removed from the project or group)."

// releaseBatch bounds how many tickets one page of a release run loads.
const releaseBatch = 100

// ReleaseIneligible clears the assignee of every open, active or waiting
// ticket assigned to userID (in projectID, or in every project when it is
// empty) whose assignee is no longer an eligible member of the ticket's
// group: the user was disabled, removed from the project or removed from
// the group. Tickets go back to their group queue unassigned (the status is
// kept). Each release is one audited transaction with an outbox
// event; the run is resumable because it only selects tickets that are
// still assigned to the user. It returns how many tickets were released.
func (s *Service) ReleaseIneligible(ctx context.Context, workspaceID, projectID, userID string, actor audit.Actor) (int, error) {
	released, after := 0, ""
	for {
		batch, err := s.repo.AssignedTickets(ctx, workspaceID, projectID, userID, after, releaseBatch)
		if err != nil {
			return released, err
		}
		if len(batch) == 0 {
			return released, nil
		}
		for _, t := range batch {
			after = t.Id
			ok, err := s.releaseIfIneligible(ctx, workspaceID, t, userID, actor)
			if err != nil {
				return released, err
			}
			if ok {
				released++
			}
		}
	}
}

func (s *Service) releaseIfIneligible(ctx context.Context, workspaceID string, t Ticket, userID string, actor audit.Actor) (bool, error) {
	for attempt := 0; attempt < maxReplyAttempts; attempt++ {
		if t.AssigneeID() != userID || t.Archived() || t.StatusCategory == statuses.CategoryResolved {
			return false, nil
		}
		eligible, err := s.repo.IsEligible(ctx, t.ProjectId, t.Group.Id, userID)
		if err != nil {
			return false, err
		}
		if eligible {
			return false, nil
		}
		all, err := s.catalog.ProjectStatuses(ctx, workspaceID, t.ProjectId)
		if err != nil {
			return false, err
		}
		set, err := NewStatusSet(all)
		if err != nil {
			return false, err
		}
		plan, err := PlanRelease(t, set)
		if err != nil {
			return false, nil // archived meanwhile
		}
		now := s.clock.Now()
		ch := Changes{UpdatedAt: now}
		changed := statusChanges(&ch, plan, t, now)
		ev := audit.New(ctx, actor, audit.TicketReleased, "ticket/"+t.Id, now)
		ev.ProjectId, ev.TicketId, ev.Changes = t.ProjectId, t.Id, changed
		ev.Reason = SystemReleaseReason
		oe := outbox.Event{Type: outbox.TicketReleased, ProjectId: t.ProjectId, TicketId: t.Id, Visibility: VisibilityAgent,
			Actor: actor, OccurredAt: now, Transition: outbox.TransitionRelease}
		_, _, err = s.repo.Mutate(ctx, Mutation{ProjectId: t.ProjectId, TicketId: t.Id, ExpectedVersion: t.Version,
			RequireAssignee: userID, RequireCategories: workableCategories(), Changes: ch,
			Audit: []audit.Event{ev}, Outbox: []outbox.Event{oe}})
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, ErrPrecondition):
			cur, gerr := s.repo.Get(ctx, t.ProjectId, t.Id, "")
			if errors.Is(gerr, ErrNotFound) {
				return false, nil
			}
			if gerr != nil {
				return false, gerr
			}
			t = cur
			continue
		case errors.Is(err, ErrNotFound):
			return false, nil
		}
		return false, err
	}
	return false, errors.New("tickets: release kept conflicting with concurrent changes; will retry")
}
