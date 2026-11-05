// Package macros manages canned replies: project-scoped templates with a
// name, a Markdown body and the audience the body is meant for.
//
// Preview renders the body for one ticket with a fixed set of safe
// variables. The reply is never sent from here: the agent inserts it into
// the composer, reviews it and posts it like any other message. The
// package and the stored collection are named "macros".
package macros

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// MaxMacros bounds the canned replies of one project.
const MaxMacros = 200

// Macro is a canned reply.
type Macro struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	Name        string
	Body        string // Markdown with {{variables}}
	Visibility  string // audience the body is meant for: public or agent
	Active      bool
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Version     int64
}

// Repository errors.
var (
	ErrNotFound = errors.New("macros: not found")
	ErrConflict = errors.New("macros: version conflict")
	ErrLimit    = errors.New("macros: limit reached")
)

// Repository persists canned replies.
type Repository interface {
	ListMacros(ctx context.Context, projectID string) ([]Macro, error)
	GetMacro(ctx context.Context, projectID, id string) (Macro, error)
	CreateMacro(ctx context.Context, m Macro, ev audit.Event) (Macro, error)
	UpdateMacro(ctx context.Context, m Macro, expectedVersion int64, ev audit.Event) (Macro, error)
	DeleteMacro(ctx context.Context, m Macro, ev audit.Event) error
}

// Tickets reads the ticket a canned reply is rendered for.
type Tickets interface {
	Get(ctx context.Context, scope *access.ProjectScope, ref string) (tickets.Ticket, error)
}

// Service implements canned replies.
type Service struct {
	repo    Repository
	tickets Tickets
	clock   clock.Clock
}

// NewService returns the canned replies service.
func NewService(repo Repository, t Tickets, clk clock.Clock) *Service {
	return &Service{repo: repo, tickets: t, clock: clk}
}

func errNotFound() error { return fault.NotFound("macroNotFound") }

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return errNotFound()
	case errors.Is(err, ErrConflict):
		return fault.Conflict("versionConflict", "The canned reply was changed by someone else. Reload and try again.")
	case errors.Is(err, ErrLimit):
		return fault.Conflict("macroLimitReached", "A project can have at most 200 canned replies.")
	}
	return err
}

func requireStaff(scope *access.ProjectScope) error {
	if err := scope.Principal.RequireStaff(); err != nil {
		return err
	}
	return scope.Require(access.CapTicketRead)
}

// ---- Variables ----

var variablePattern = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// Variables lists the supported template variables.
func Variables() []string { return []string{"requester.name", "ticket.number", "agent.name"} }

// markdownSpecial are escaped in substituted values so a requester name
// cannot inject links, emphasis or HTML into the rendered Markdown.
var markdownSpecial = `\` + "`" + `*_{}[]()#!|<>~&`

func escapeMarkdown(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
		case strings.ContainsRune(markdownSpecial, r):
			b.WriteRune('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Render substitutes the supported variables. Values are escaped for
// Markdown, except the ticket reference ("#" and digits, never a heading
// because no space follows "#"); text outside variables is the template
// author's Markdown.
func Render(body string, t tickets.Ticket, agentName string) string {
	values := map[string]string{"requester.name": t.Requester.Name, "ticket.number": t.Ref(), "agent.name": agentName}
	return variablePattern.ReplaceAllStringFunc(body, func(m string) string {
		name := strings.TrimSpace(variablePattern.FindStringSubmatch(m)[1])
		v, ok := values[name]
		if !ok {
			return m
		}
		if name == "ticket.number" {
			return v
		}
		return escapeMarkdown(v)
	})
}

// ---- Administration ----

// Input creates or changes a canned reply.
type Input struct {
	Name       *string
	Body       *string
	Visibility *string
	Active     *bool
}

func validate(m Macro) error {
	var v validation.Collector
	v.String(validation.MacroName, "name", m.Name)
	v.String(validation.MacroBody, "body", m.Body)
	v.String(validation.MessageVisibility, "visibility", m.Visibility)
	for _, match := range variablePattern.FindAllStringSubmatch(m.Body, -1) {
		if name := strings.TrimSpace(match[1]); !slices.Contains(Variables(), name) {
			v.Add("body", "unknownVariable", "Unknown variable {{"+name+"}}. Use "+strings.Join(Variables(), ", ")+".")
		}
	}
	if strings.TrimSpace(m.Body) == "" {
		v.Add("body", validation.CodeRequired, "Add the reply text.")
	}
	return v.Err()
}

// List returns the project's canned replies (staff).
func (s *Service) List(ctx context.Context, scope *access.ProjectScope) ([]Macro, error) {
	if err := requireStaff(scope); err != nil {
		return nil, err
	}
	return s.repo.ListMacros(ctx, scope.Project.Id)
}

// Get returns one canned reply (staff).
func (s *Service) Get(ctx context.Context, scope *access.ProjectScope, id string) (Macro, error) {
	if err := requireStaff(scope); err != nil {
		return Macro{}, err
	}
	if !validation.ValidID(id) {
		return Macro{}, errNotFound()
	}
	m, err := s.repo.GetMacro(ctx, scope.Project.Id, id)
	return m, mapErr(err)
}

// Create adds a canned reply (administrators).
func (s *Service) Create(ctx context.Context, scope *access.ProjectScope, in Input) (Macro, error) {
	if err := requireStaff(scope); err != nil {
		return Macro{}, err
	}
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Macro{}, err
	}
	now := s.clock.Now()
	m := Macro{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, Visibility: tickets.VisibilityPublic, Active: true,
		CreatedBy: scope.Principal.Id, CreatedAt: now, UpdatedAt: now, Version: 1}
	apply(&m, in)
	if err := validate(m); err != nil {
		return Macro{}, err
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.MacroCreated, "", now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name", "body", "visibility", "active"}
	out, err := s.repo.CreateMacro(ctx, m, ev)
	return out, mapErr(err)
}

func apply(m *Macro, in Input) []string {
	var changed []string
	if in.Name != nil && *in.Name != m.Name {
		m.Name, changed = *in.Name, append(changed, "name")
	}
	if in.Body != nil && *in.Body != m.Body {
		m.Body, changed = *in.Body, append(changed, "body")
	}
	if in.Visibility != nil && *in.Visibility != m.Visibility {
		m.Visibility, changed = *in.Visibility, append(changed, "visibility")
	}
	if in.Active != nil && *in.Active != m.Active {
		m.Active, changed = *in.Active, append(changed, "active")
	}
	return changed
}

// Update changes a canned reply with If-Match (administrators).
func (s *Service) Update(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64, in Input) (Macro, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Macro{}, err
	}
	m, err := s.Get(ctx, scope, id)
	if err != nil {
		return Macro{}, err
	}
	changed := apply(&m, in)
	if err := validate(m); err != nil {
		return Macro{}, err
	}
	if m.Version != expectedVersion {
		return Macro{}, mapErr(ErrConflict)
	}
	if len(changed) == 0 {
		return m, nil
	}
	now := s.clock.Now()
	m.UpdatedAt = now
	ev := audit.New(ctx, scope.AuditActor(), audit.MacroUpdated, "macro/"+m.Id, now)
	ev.ProjectId, ev.Changes = scope.Project.Id, changed
	out, err := s.repo.UpdateMacro(ctx, m, expectedVersion, ev)
	return out, mapErr(err)
}

// Delete removes a canned reply (administrators).
func (s *Service) Delete(ctx context.Context, scope *access.ProjectScope, id string) error {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return err
	}
	m, err := s.Get(ctx, scope, id)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	ev := audit.New(ctx, scope.AuditActor(), audit.MacroDeleted, "macro/"+m.Id, now)
	ev.ProjectId = scope.Project.Id
	return mapErr(s.repo.DeleteMacro(ctx, m, ev))
}

// ---- Preview ----

// Preview is a canned reply rendered for one ticket.
type Preview struct {
	Macro      Macro
	Body       string
	Visibility string
}

// Preview renders the canned reply for a ticket the caller can read.
func (s *Service) Preview(ctx context.Context, scope *access.ProjectScope, ticketRef, macroID string) (Preview, error) {
	m, err := s.Get(ctx, scope, macroID)
	if err != nil {
		return Preview{}, err
	}
	t, err := s.tickets.Get(ctx, scope, ticketRef)
	if err != nil {
		return Preview{}, err
	}
	return Preview{Macro: m, Body: Render(m.Body, t, scope.Principal.Name), Visibility: m.Visibility}, nil
}
