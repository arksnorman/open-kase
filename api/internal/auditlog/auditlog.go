// Package auditlog lists the installation's audit events for administrators
// (the Audit log tab of the admin Logs area). Audit events record field
// names and identifiers only, never message bodies or secret values, so the
// listing returns what is stored plus display names looked up per page.
package auditlog

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Actor is who performed an action, with a display name.
type Actor struct {
	Type   string // audit.ListedUser, audit.ListedToken or audit.ListedSystem
	Id     string
	Name   string
	Label  string // stored non-secret description (system and CLI actors)
	Scopes []string
}

// TicketRef identifies a ticket target.
type TicketRef struct {
	Id      string
	Number  int64
	Subject string
}

// Target is what an action was applied to.
type Target struct {
	Type   string // e.g. "ticket", "user", "settings"
	Id     string
	Name   string
	Ticket *TicketRef
}

// Entry is one audit event as listed.
type Entry struct {
	Id        string
	CreatedAt time.Time
	Action    string
	Actor     Actor
	Target    *Target
	RequestId string
	Changes   []string
	Reason    string
	Outcome   string
}

// Filter selects audit events (newest first).
type Filter struct {
	WorkspaceId  string
	ActorTypes   []string // stored actor types
	ActorId      string   // user/token ID, or the system actor kind
	Action       string   // exact code
	ActionPrefix string
	TicketId     string
	From, To     *time.Time // To is exclusive
	After        *pagination.Cursor
	Limit        int
}

// Repository reads audit events and resolves display names.
type Repository interface {
	List(ctx context.Context, f Filter) ([]Entry, error)
	// TicketID resolves a ticket by number; ok is false when none exists.
	TicketID(ctx context.Context, workspaceID string, number int64) (string, bool, error)
	// Actors returns up to limit users and up to limit integration tokens
	// whose name (users: also username or email) contains q, ignoring case,
	// each sorted by name; truncated reports that either list had more.
	Actors(ctx context.Context, workspaceID, q string, limit int) (items []ActorOption, truncated bool, err error)
}

// ActorOption is one entry of the actor catalog (the audit log's actor filter).
type ActorOption struct {
	Type   string // audit.ListedUser, audit.ListedToken or audit.ListedSystem
	Id     string // user or token ID, or the system actor kind
	Name   string
	Detail string // username and email, token state, or what the system actor does
}

// Service lists audit events.
type Service struct{ repo Repository }

// NewService returns the audit log service.
func NewService(r Repository) *Service { return &Service{repo: r} }

// Query is the listing input as received from a transport.
type Query struct {
	ActorType string
	ActorId   string
	Action    string
	Ticket    string // ticket ID or number ("12" or "#12")
	From, To  string // YYYY-MM-DD, inclusive, UTC
	After     *pagination.Cursor
	Limit     int
}

const day = 24 * time.Hour

// List pages through audit events, newest first (administrators).
func (s *Service) List(ctx context.Context, p access.Principal, q Query) (pagination.Page[Entry], error) {
	if err := p.RequireAdmin(); err != nil {
		return pagination.Page[Entry]{}, err
	}
	var v validation.Collector
	if q.ActorType != "" {
		v.String(validation.AuditActorType, "actorType", q.ActorType)
	}
	if q.ActorId != "" {
		if q.ActorType == audit.ListedSystem {
			if !audit.KnownSystemActor(q.ActorId) {
				v.Add("actorId", validation.CodeInvalidChoice, "Choose a system actor listed by GET /audit-actors.")
			}
		} else {
			v.String(validation.ObjectID, "actorId", q.ActorId)
		}
	}
	if q.Action != "" {
		v.String(validation.AuditAction, "action", q.Action)
	}
	if q.Ticket != "" {
		v.String(validation.AuditTicket, "ticket", q.Ticket)
	}
	if q.From != "" {
		v.String(validation.AuditDate, "from", q.From)
	}
	if q.To != "" {
		v.String(validation.AuditDate, "to", q.To)
	}
	if err := v.Err(); err != nil {
		return pagination.Page[Entry]{}, err
	}
	f := Filter{WorkspaceId: p.WorkspaceId, ActorId: q.ActorId, ActorTypes: audit.StoredTypes(q.ActorType), After: q.After, Limit: q.Limit + 1}
	// An action ending in "." is a prefix (every ticket.* action); anything
	// else is an exact code from GET /audit-actions.
	if strings.HasSuffix(q.Action, ".") {
		f.ActionPrefix = q.Action
	} else {
		f.Action = q.Action
	}
	if q.From != "" {
		t, err := time.Parse(time.DateOnly, q.From)
		if err != nil {
			return pagination.Page[Entry]{}, fault.Field("from", validation.CodeInvalidFormat, "Use a valid date.")
		}
		f.From = &t
	}
	if q.To != "" {
		t, err := time.Parse(time.DateOnly, q.To)
		if err != nil {
			return pagination.Page[Entry]{}, fault.Field("to", validation.CodeInvalidFormat, "Use a valid date.")
		}
		t = t.Add(day)
		f.To = &t
	}
	if f.From != nil && f.To != nil && !f.To.After(*f.From) {
		return pagination.Page[Entry]{}, fault.Field("to", validation.CodeOutOfRange, "The end date must not be before the start date.")
	}
	if q.After != nil {
		if _, err := time.Parse(time.RFC3339Nano, q.After.Sort); err != nil || !validation.ValidID(q.After.Id) {
			return pagination.Page[Entry]{}, fault.Validation(fault.FieldError{Field: "cursor", Code: "invalidCursor", Message: "The page cursor is invalid or expired."})
		}
	}
	if q.Ticket != "" {
		if validation.ValidID(q.Ticket) {
			f.TicketId = q.Ticket
		} else {
			// "1042" or "#1042".
			n, err := strconv.ParseInt(strings.TrimPrefix(q.Ticket, "#"), 10, 64)
			if err != nil || n < 1 {
				return pagination.Page[Entry]{}, fault.Field("ticket", validation.CodeInvalidFormat, "Use a ticket number or ID.")
			}
			id, ok, err := s.repo.TicketID(ctx, p.WorkspaceId, n)
			if err != nil {
				return pagination.Page[Entry]{}, err
			}
			if !ok {
				return pagination.Page[Entry]{Items: []Entry{}}, nil
			}
			f.TicketId = id
		}
	}
	rows, err := s.repo.List(ctx, f)
	if err != nil {
		return pagination.Page[Entry]{}, err
	}
	return pagination.NewPage(rows, q.Limit, func(e Entry) pagination.Cursor {
		return pagination.Cursor{Sort: e.CreatedAt.UTC().Format(time.RFC3339Nano), Id: e.Id}
	}), nil
}

// Actions returns the action catalog (administrators).
func (s *Service) Actions(p access.Principal) ([]audit.ActionInfo, error) {
	if err := p.RequireAdmin(); err != nil {
		return nil, err
	}
	return audit.Actions(), nil
}

// ActorQuery is the actor catalog input.
type ActorQuery struct {
	Q     string
	Limit *int // 1–200; nil means 50
}

// Actors lists who can appear as an audit actor (administrators): users
// (deactivated and invited ones too), integration tokens (revoked ones too)
// and the fixed system actors, filtered by Q. Users come first, then tokens,
// then system actors; at most Limit users and Limit tokens are returned.
// Webhook endpoints are not listed: they deliver events but never perform
// audited actions.
func (s *Service) Actors(ctx context.Context, p access.Principal, q ActorQuery) ([]ActorOption, bool, error) {
	if err := p.RequireAdmin(); err != nil {
		return nil, false, err
	}
	q.Q = strings.TrimSpace(q.Q)
	limit := 50
	if q.Limit != nil {
		limit = *q.Limit
	}
	var v validation.Collector
	if q.Q != "" {
		v.String(validation.AuditActorQuery, "q", q.Q)
	}
	v.Int(validation.AuditActorLimit, "limit", int64(limit))
	if err := v.Err(); err != nil {
		return nil, false, err
	}
	out, truncated, err := s.repo.Actors(ctx, p.WorkspaceId, q.Q, limit)
	if err != nil {
		return nil, false, err
	}
	needle := strings.ToLower(q.Q)
	for _, sa := range audit.SystemActors() {
		if needle == "" || strings.Contains(strings.ToLower(sa.Name+" "+sa.Detail), needle) {
			out = append(out, ActorOption{Type: audit.ListedSystem, Id: sa.Id, Name: sa.Name, Detail: sa.Detail})
		}
	}
	return out, truncated, nil
}
