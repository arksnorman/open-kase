package tickets

import (
	"errors"

	"github.com/arksnorman/open-kase/api/internal/statuses"
)

// Lifecycle errors (architecture §3.3).
var (
	ErrArchived          = errors.New("tickets: archived tickets cannot change")
	ErrInvalidTransition = errors.New("tickets: transition not allowed from the current status")
	ErrStatusNotAllowed  = errors.New("tickets: status cannot be selected with setStatus")
)

// StatusSet is a project's status catalog with the protected built-ins
// resolved by code.
type StatusSet struct {
	Open, OnHold, Resolved, Archived statuses.Status
	byID                             map[string]statuses.Status
}

// NewStatusSet indexes a catalog. It fails when a built-in is missing,
// which means the installation was not initialized correctly.
func NewStatusSet(all []statuses.Status) (StatusSet, error) {
	s := StatusSet{byID: make(map[string]statuses.Status, len(all))}
	for _, st := range all {
		s.byID[st.Id] = st
		if !st.Builtin {
			continue
		}
		switch st.Code {
		case statuses.CodeOpen:
			s.Open = st
		case statuses.CodeOnHold:
			s.OnHold = st
		case statuses.CodeResolved:
			s.Resolved = st
		case statuses.CodeArchived:
			s.Archived = st
		}
	}
	if s.Open.Id == "" || s.OnHold.Id == "" || s.Resolved.Id == "" || s.Archived.Id == "" {
		return StatusSet{}, errors.New("tickets: built-in statuses are missing; run init")
	}
	return s, nil
}

// ByID returns an active status of the project.
func (s StatusSet) ByID(id string) (statuses.Status, bool) {
	st, ok := s.byID[id]
	return st, ok
}

// Plan is the outcome of a lifecycle decision.
type Plan struct {
	Status        statuses.Status
	ClearAssignee bool
	SetResolved   bool // record resolvedAt = now
	ClearResolved bool
	SetArchived   bool
	// Hold is set when the plan puts the ticket on hold (the service fills
	// it from the chosen hold reason).
	Hold *Hold
}

// Changed reports whether the plan changes the ticket's status or assignee.
func (p Plan) Changed(t Ticket) bool {
	return p.Status.Id != t.Status.Id || (p.ClearAssignee && t.Assignee != nil)
}

func keep(t Ticket) Plan {
	return Plan{Status: statuses.Status{Id: t.Status.Id, Name: t.Status.Name, Category: t.StatusCategory}}
}

// PlanClaim returns the status after a claim: the status is preserved (an
// Open ticket stays Open, now with the assignee).
func PlanClaim(t Ticket, _ StatusSet) (Plan, error) {
	if t.Archived() {
		return Plan{}, ErrArchived
	}
	if !t.StatusCategory.Workable() {
		return Plan{}, ErrInvalidTransition
	}
	return keep(t), nil
}

// PlanRelease returns the status after releasing the assignee: the status
// is preserved.
func PlanRelease(t Ticket, _ StatusSet) (Plan, error) {
	if t.Archived() {
		return Plan{}, ErrArchived
	}
	p := keep(t)
	p.ClearAssignee = true
	return p, nil
}

// PlanAssignment returns the status after an assignment or transfer: the
// status is preserved; an empty newAssignee clears the assignee.
func PlanAssignment(t Ticket, _ StatusSet, newAssignee string) (Plan, error) {
	if t.Archived() {
		return Plan{}, ErrArchived
	}
	p := keep(t)
	if newAssignee == "" {
		p.ClearAssignee = true
	}
	return p, nil
}

// PlanTransition applies an explicit lifecycle action. assigneeEligible
// reports whether the current assignee is still an active member of the
// ticket's group (used by reopen).
func PlanTransition(t Ticket, action string, target *statuses.Status, set StatusSet, assigneeEligible bool) (Plan, error) {
	if t.Archived() {
		return Plan{}, ErrArchived
	}
	switch action {
	case "resolve":
		if !t.StatusCategory.Workable() {
			return Plan{}, ErrInvalidTransition
		}
		return Plan{Status: set.Resolved, SetResolved: true}, nil
	case "reopen":
		if t.StatusCategory != statuses.CategoryResolved {
			return Plan{}, ErrInvalidTransition
		}
		return reopen(t, set, assigneeEligible), nil
	case "archive":
		if t.StatusCategory != statuses.CategoryResolved {
			return Plan{}, ErrInvalidTransition
		}
		return Plan{Status: set.Archived, SetArchived: true}, nil
	case "hold":
		// Only an Open or Awaiting customer ticket can be put on hold.
		if t.StatusCategory != statuses.CategoryOpen && t.StatusCategory != statuses.CategoryWaitingCustomer {
			return Plan{}, ErrInvalidTransition
		}
		return Plan{Status: set.OnHold}, nil
	case "setStatus":
		// On hold is entered with the hold action (it needs a reason).
		if target == nil || !target.Category.Workable() || target.Category == statuses.CategoryOnHold {
			return Plan{}, ErrStatusNotAllowed
		}
		if !t.StatusCategory.Workable() {
			return Plan{}, ErrInvalidTransition
		}
		return Plan{Status: *target}, nil
	}
	return Plan{}, ErrInvalidTransition
}

func reopen(t Ticket, set StatusSet, assigneeEligible bool) Plan {
	return Plan{Status: set.Open, ClearAssignee: t.Assignee != nil && !assigneeEligible, ClearResolved: true}
}

// PlanRequesterReply applies the requester's public reply: a ticket that
// is waiting for the customer, on hold or resolved goes back to Open,
// keeping an eligible assignee (an ineligible one is cleared). Other
// statuses are preserved.
func PlanRequesterReply(t Ticket, set StatusSet, assigneeEligible bool) (Plan, error) {
	if t.Archived() {
		return Plan{}, ErrArchived
	}
	switch t.StatusCategory {
	case statuses.CategoryWaitingCustomer, statuses.CategoryOnHold, statuses.CategoryResolved:
		return reopen(t, set, assigneeEligible), nil
	}
	return keep(t), nil
}
