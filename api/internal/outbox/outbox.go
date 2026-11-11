// Package outbox defines durable domain events that are committed in the
// same MongoDB transaction as the business change that caused them. A
// dispatcher (Milestone 5) fans them out to email and webhook jobs; until
// then they accumulate with state "pending".
//
// Events carry identifiers and the audience of the change, never message
// bodies: workers load the current authorized projection when they run.
package outbox

import (
	"context"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
)

// SchemaVersion is the version of the event document shape.
const SchemaVersion = 1

// Event types.
const (
	TicketCreated       = "ticket.created"
	TicketUpdated       = "ticket.updated"
	TicketClaimed       = "ticket.claimed"
	TicketReleased      = "ticket.released"
	TicketAssigned      = "ticket.assigned"
	TicketStatusChanged = "ticket.statusChanged"
	MessageCreated      = "message.created"
	// SLA timer events (Milestone 6): a target is due soon or breached.
	TicketSLAWarning  = "ticket.slaWarning"
	TicketSLABreached = "ticket.slaBreached"

	// Ticket workflow: link changes and message redaction are
	// staff-side invalidations (no email, no webhook); a follow-up reminder
	// emails the requester contact.
	TicketLinkAdded        = "ticket.linkAdded"
	TicketLinkRemoved      = "ticket.linkRemoved"
	MessageRedacted        = "message.redacted"
	TicketFollowupReminder = "ticket.followupReminder"

	// Scheduled messages (staff-only, never webhooks): a scheduled message
	// was held for the author's review (emails the author) or cancelled by
	// the system (live refresh only). UserID is the author.
	ScheduledMessageNeedsReview = "scheduledMessage.needsReview"
	ScheduledMessageCancelled   = "scheduledMessage.cancelled"

	// Access changes that require releasing assignments (no notification).
	GroupMembershipRemoved = "groupMembership.removed"
	UserDisabled           = "user.disabled"
)

// Transitions recorded on ticket.statusChanged events.
const (
	TransitionResolve        = "resolve"
	TransitionReopen         = "reopen"
	TransitionArchive        = "archive"
	TransitionSetStatus      = "setStatus"
	TransitionRequesterReply = "requesterReply"
	TransitionRelease        = "release"
	TransitionHold           = "hold"
	// TransitionHoldEnded: the hold ran out and automation reopened the ticket.
	TransitionHoldEnded = "holdEnded"
)

// Event is one durable domain event.
type Event struct {
	Type       string
	ProjectId  string
	TicketId   string // filled by the repository for creates
	MessageId  string // filled by the repository when a message is inserted
	Visibility string // audience of the change: "public" or "agent"
	Actor      audit.Actor
	OccurredAt time.Time
	// Transition names the lifecycle action of a status change.
	Transition string
	// Changes lists the public field names of a ticket.updated event. Other
	// event types carry marks in it, which are never sent as field names:
	// contact:<id> (ticket.created, ticket.statusChanged), holdReason:<name>
	// (hold ended), reminder:<n> and autoResolveNotice (follow-up reminder),
	// and the reason and visibility:<audience> of a scheduled-message notice.
	Changes []string
	// UserID and GroupID identify the subject of an access-change event.
	UserId  string
	GroupId string
	// SLAKind and SLADueAt describe an SLA warning or breach.
	SLAKind  string
	SLADueAt *time.Time
}

// Snapshot is the ticket state right after the change, recorded with the
// event so recipients and payloads reflect the event, not a later state.
type Snapshot struct {
	Subject        string
	Priority       int
	StatusId       string
	StatusCategory string
	GroupId        string
	AssigneeId     string
	RequesterId    string
	RequestTypeId  string
	// OrganisationId is the ticket's organisation ("" when none).
	OrganisationId string
}

// Record is a stored event as the dispatcher reads it.
type Record struct {
	Id            string
	WorkspaceId   string
	ProjectId     string
	TicketId      string
	MessageId     string
	Type          string
	Visibility    string
	TicketVersion int64
	Actor         audit.Actor
	// MessageAuthorKind is "user", "integration" (an agent token) or "contact"
	// for message events.
	MessageAuthorKind string
	MessageAuthorId   string
	Transition        string
	Changes           []string
	UserId            string
	GroupId           string
	SLAKind           string
	SLADueAt          *time.Time
	Snapshot          Snapshot
	State             string
	OccurredAt        time.Time
	CreatedAt         time.Time
	// LeaseGeneration fences the dispatcher that claimed the record.
	LeaseGeneration int64
}

// Outbox states.
const (
	StatePending    = "pending"
	StateDispatched = "dispatched"
	StateSuppressed = "suppressed"
	StateFailed     = "failed"
)

type suppressKey struct{}

// Suppress marks events written under ctx as suppressed: they are stored
// for history but never dispatched. Used by the demo seed so fixtures do
// not email fictional contacts once delivery exists.
func Suppress(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressKey{}, true)
}

// Suppressed reports whether events written under ctx must not be dispatched.
func Suppressed(ctx context.Context) bool {
	v, _ := ctx.Value(suppressKey{}).(bool)
	return v
}
