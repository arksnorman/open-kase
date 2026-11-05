// Package audit defines append-only audit events. Audit events are business
// history and are stored separately from diagnostic logs. They record field
// names and identifiers, never secret values or private message bodies.
package audit

import (
	"context"
	"time"
)

// Actor types.
const (
	ActorUser     = "user"
	ActorService  = "service" // integration service principal
	ActorOperator = "operator"
	ActorSystem   = "system"
)

// Actor identifies who performed an action.
type Actor struct {
	Type  string // "user", "service", "operator", "system"
	Id    string // user/service principal ID; the system actor kind (SystemAutomation, …) for system and operator actors
	Label string // non-secret description, e.g. "cli init (os user root)"
	// Scopes are the integration token scopes that allowed the action
	// (integration tokens only).
	Scopes []string
}

// Event is one audit record.
type Event struct {
	WorkspaceId string
	ProjectId   string
	TicketId    string
	Action      Action
	Actor       Actor
	RequestId   string
	Target      string   // e.g. "settings/installation" or "user/<id>"
	Changes     []string // changed field paths; values are not recorded
	Reason      string
	Outcome     string // "success" or "failure"
	CreatedAt   time.Time
}

// Operator returns an actor for a local CLI operator of the given system
// actor kind (SystemInstallation, SystemCLI).
func Operator(kind, label string) Actor { return Actor{Type: ActorOperator, Id: kind, Label: label} }

// System returns an actor for automatic server actions of the given system
// actor kind (SystemAutomation, SystemSLA, …).
func System(kind, label string) Actor { return Actor{Type: ActorSystem, Id: kind, Label: label} }

type requestIDKey struct{}

// WithRequestID stores the transport request ID so services can attach it to
// audit events without depending on a transport.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request ID stored by WithRequestID, if any.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// New builds a successful event stamped with the context's request ID.
func New(ctx context.Context, actor Actor, action Action, target string, now time.Time) Event {
	return Event{Action: action, Actor: actor, Target: target, RequestId: RequestID(ctx), Outcome: "success", CreatedAt: now}
}
