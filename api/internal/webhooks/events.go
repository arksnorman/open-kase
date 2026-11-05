package webhooks

import "slices"

// Webhook event types an endpoint can select.
const (
	TypeTicketCreated       = "ticket.created"
	TypeTicketUpdated       = "ticket.updated"
	TypeMessagePublic       = "ticket.message.public"
	TypeMessageAgentNote    = "ticket.message.agentNote"
	TypeTicketAssigned      = "ticket.assigned"
	TypeTicketStatusChanged = "ticket.statusChanged"
	TypeSLAWarning          = "ticket.slaWarning"
	TypeSLABreached         = "ticket.slaBreached"
	// TypeTest is sent only by the "test delivery" action.
	TypeTest = "webhook.test"
)

// EventTypes lists the selectable event types.
func EventTypes() []string {
	return []string{TypeTicketCreated, TypeTicketUpdated, TypeMessagePublic, TypeMessageAgentNote,
		TypeTicketAssigned, TypeTicketStatusChanged, TypeSLAWarning, TypeSLABreached}
}

// ValidEventType reports whether t can be selected for an endpoint.
func ValidEventType(t string) bool { return slices.Contains(EventTypes(), t) }

// EventTypeFor maps a stored domain event (outbox type and audience) to the
// webhook event type, or "" when the event is not exposed as a webhook.
func EventTypeFor(outboxType, visibility string) string {
	switch outboxType {
	case "ticket.created":
		return TypeTicketCreated
	case "ticket.updated":
		return TypeTicketUpdated
	case "message.created":
		if visibility == "agent" {
			return TypeMessageAgentNote
		}
		return TypeMessagePublic
	case "ticket.claimed", "ticket.released", "ticket.assigned":
		return TypeTicketAssigned
	case "ticket.statusChanged":
		return TypeTicketStatusChanged
	case "ticket.slaWarning":
		return TypeSLAWarning
	case "ticket.slaBreached":
		return TypeSLABreached
	}
	return ""
}
