// Package requesttypes manages the request types a ticket can carry. Admins
// create, rename, reorder, deactivate, reactivate and delete them; every
// staff member and every token can read them. A type has a code that never
// changes, so integrations can map to it by code while the name is edited.
// The stored collection is named "requestTypes".
package requesttypes

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// MaxTypes bounds the request types of one project, active and inactive.
const MaxTypes = 50

// orderStep is the gap between the orders of consecutive types.
const orderStep = 10

// Type is a request type.
type Type struct {
	Id     string
	Code   string
	Name   string
	Order  int
	Active bool
	// InUse is the number of tickets carrying the type, in any status. It is
	// filled by List for callers who manage request types only.
	InUse     int64
	CreatedAt time.Time
	// Version is 1 for a type that was never changed (also when the stored
	// document has no version).
	Version int64
}

// NormalizeName is the key of the per-project unique name.
func NormalizeName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// Repository errors.
var (
	ErrNotFound      = errors.New("requestTypes: not found")
	ErrConflict      = errors.New("requestTypes: version conflict")
	ErrExists        = errors.New("requestTypes: name exists")
	ErrCodeExists    = errors.New("requestTypes: code exists")
	ErrLimit         = errors.New("requestTypes: limit reached")
	ErrInUse         = errors.New("requestTypes: in use")
	ErrLastActive    = errors.New("requestTypes: last active type")
	ErrOrderConflict = errors.New("requestTypes: order conflict")
)

// Update is one stored change of an existing type. Only the non-nil fields
// are written, and Version becomes the stored version.
type Update struct {
	Id      string
	Name    *string
	Active  *bool
	Order   *int
	Version int64
}

// Plan is what one management command writes. A zero Plan writes nothing.
type Plan struct {
	Insert *Type
	Update []Update
	// Delete is the ID of a type to remove; the repository refuses it with
	// ErrInUse while a ticket carries the type.
	Delete string
	// Audit is written with the change (empty Target: the inserted type).
	Audit *audit.Event
}

// Repository persists request types.
type Repository interface {
	// List returns every type of the project, active and inactive, by order;
	// withUsage fills InUse.
	List(ctx context.Context, workspaceID, projectID string, withUsage bool) ([]Type, error)
	// Change runs one management command in a transaction that serializes
	// commands per project: it reads the project's types, calls plan with
	// them (plan may run more than once and must not have side effects),
	// applies the plan and its audit event, and returns the types after the
	// change with usage filled.
	Change(ctx context.Context, workspaceID, projectID string, plan func(current []Type) (Plan, error)) ([]Type, error)
}

// Service implements request type reading and management.
type Service struct {
	repo  Repository
	clock clock.Clock
}

// NewService returns the request types service.
func NewService(repo Repository, clk clock.Clock) *Service { return &Service{repo: repo, clock: clk} }

func errNotFound() error { return fault.NotFound("requestTypeNotFound") }

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return errNotFound()
	case errors.Is(err, ErrConflict):
		return fault.Conflict("versionConflict", "The request type was changed by someone else. Reload and try again.")
	case errors.Is(err, ErrExists):
		return fault.Conflict("requestTypeExists", "A request type with this name already exists.")
	case errors.Is(err, ErrCodeExists):
		return fault.Conflict("requestTypeCodeExists", "A request type with this code already exists.")
	case errors.Is(err, ErrLimit):
		return fault.Conflict("requestTypeLimitReached", "A project can have at most 50 request types.")
	case errors.Is(err, ErrInUse):
		return fault.Conflict("requestTypeInUse", "Tickets carry this request type. Deactivate it instead.")
	case errors.Is(err, ErrLastActive):
		return fault.Conflict("requestTypeLastActive", "At least one request type must stay active.")
	case errors.Is(err, ErrOrderConflict):
		return fault.Conflict("requestTypeOrderConflict", "The list of request types changed. Reload and try again.")
	}
	return err
}

// Types returns every request type of the project, active and inactive, by
// order, without an access check. Ticket commands use it to resolve and
// default the request type; callers check access themselves.
func (s *Service) Types(ctx context.Context, workspaceID, projectID string) ([]Type, error) {
	return s.repo.List(ctx, workspaceID, projectID, false)
}

// List returns the project's request types by order (users and tokens that
// can read the project). active is "true" (the default), "false" or "all".
// inUse counts are reported to callers who manage request types.
func (s *Service) List(ctx context.Context, scope *access.ProjectScope, active string) ([]Type, error) {
	if err := scope.Require(access.CapProjectRead); err != nil {
		return nil, err
	}
	if active == "" {
		active = "true"
	}
	var v validation.Collector
	v.String(validation.RequestTypeActiveFilter, "active", active)
	if err := v.Err(); err != nil {
		return nil, err
	}
	all, err := s.repo.List(ctx, scope.Principal.WorkspaceId, scope.Project.Id, scope.Can(access.CapProjectManage))
	if err != nil {
		return nil, err
	}
	if active == "all" {
		return all, nil
	}
	want := active == "true"
	return slices.DeleteFunc(all, func(t Type) bool { return t.Active != want }), nil
}

// CreateInput creates a request type.
type CreateInput struct {
	Name string
	// Code is optional; it is derived from the name when empty.
	Code string
}

// UpdateInput changes a request type; nil fields stay as they are.
type UpdateInput struct {
	Name   *string
	Active *bool
}

func find(all []Type, id string) (Type, bool) {
	i := slices.IndexFunc(all, func(t Type) bool { return t.Id == id })
	if i < 0 {
		return Type{}, false
	}
	return all[i], true
}

func (s *Service) event(ctx context.Context, scope *access.ProjectScope, action audit.Action, target string, changes ...string) *audit.Event {
	ev := audit.New(ctx, scope.AuditActor(), action, target, s.clock.Now())
	ev.ProjectId, ev.Changes = scope.Project.Id, changes
	return &ev
}

// Create adds an active request type at the end of the order (admins).
func (s *Service) Create(ctx context.Context, scope *access.ProjectScope, in CreateInput) (Type, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Type{}, err
	}
	name, code := strings.TrimSpace(in.Name), strings.TrimSpace(in.Code)
	derived := code == ""
	var v validation.Collector
	v.String(validation.RequestTypeName, "name", name)
	if derived {
		if code = DeriveCode(name); validation.RequestTypeCode.CheckString("code", code) != nil && name != "" {
			v.Add("code", validation.CodeInvalidFormat, "A code cannot be derived from this name. Enter a code of 2 to 40 letters and digits that starts with a lowercase letter.")
		}
	} else {
		v.String(validation.RequestTypeCode, "code", code)
	}
	if err := v.Err(); err != nil {
		return Type{}, err
	}
	now := s.clock.Now()
	var created Type
	_, err := s.repo.Change(ctx, scope.Principal.WorkspaceId, scope.Project.Id, func(cur []Type) (Plan, error) {
		if len(cur) >= MaxTypes {
			return Plan{}, ErrLimit
		}
		key, order := NormalizeName(name), orderStep
		taken := map[string]bool{}
		for _, t := range cur {
			if NormalizeName(t.Name) == key {
				return Plan{}, ErrExists
			}
			taken[t.Code] = true
			order = max(order, t.Order+orderStep)
		}
		final := code
		if taken[final] {
			if !derived {
				return Plan{}, ErrCodeExists
			}
			for n := 2; taken[final]; n++ {
				suffix := strconv.Itoa(n)
				final = code[:min(len(code), 40-len(suffix))] + suffix
			}
		}
		created = Type{Code: final, Name: name, Order: order, Active: true, CreatedAt: now, Version: 1}
		return Plan{Insert: &created, Audit: s.event(ctx, scope, audit.RequestTypeCreated, "", "name", "code")}, nil
	})
	if err != nil {
		return Type{}, mapErr(err)
	}
	return created, nil
}

// Update renames, deactivates or reactivates a request type with If-Match
// (admins). A request that changes nothing returns the type unchanged.
func (s *Service) Update(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64, in UpdateInput) (Type, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Type{}, err
	}
	if !validation.ValidID(id) {
		return Type{}, errNotFound()
	}
	var name *string
	var v validation.Collector
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		name = &n
		v.String(validation.RequestTypeName, "name", n)
	}
	if err := v.Err(); err != nil {
		return Type{}, err
	}
	after, err := s.repo.Change(ctx, scope.Principal.WorkspaceId, scope.Project.Id, func(cur []Type) (Plan, error) {
		t, ok := find(cur, id)
		if !ok {
			return Plan{}, ErrNotFound
		}
		if t.Version != expectedVersion {
			return Plan{}, ErrConflict
		}
		u := Update{Id: id, Version: t.Version + 1}
		var changed []string
		if name != nil && *name != t.Name {
			key := NormalizeName(*name)
			for _, o := range cur {
				if o.Id != id && NormalizeName(o.Name) == key {
					return Plan{}, ErrExists
				}
			}
			u.Name, changed = name, append(changed, "name")
		}
		if in.Active != nil && *in.Active != t.Active {
			if !*in.Active && countActive(cur) == 1 {
				return Plan{}, ErrLastActive
			}
			u.Active, changed = in.Active, append(changed, "active")
		}
		if len(changed) == 0 {
			return Plan{}, nil
		}
		return Plan{Update: []Update{u}, Audit: s.event(ctx, scope, audit.RequestTypeUpdated, "requestType/"+id, changed...)}, nil
	})
	if err != nil {
		return Type{}, mapErr(err)
	}
	t, _ := find(after, id)
	return t, nil
}

func countActive(all []Type) int {
	n := 0
	for _, t := range all {
		if t.Active {
			n++
		}
	}
	return n
}

// Reorder puts every request type of the project, active and inactive, in
// the given order (admins) and returns them.
func (s *Service) Reorder(ctx context.Context, scope *access.ProjectScope, ids []string) ([]Type, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return nil, err
	}
	var v validation.Collector
	seen := map[string]bool{}
	for _, id := range ids {
		switch {
		case !validation.ValidID(id):
			v.Add("ids", validation.CodeInvalidFormat, "Every entry must be a request type ID.")
		case seen[id]:
			v.Add("ids", "duplicate", "Every request type must be listed once.")
		}
		seen[id] = true
	}
	if len(ids) == 0 {
		v.Add("ids", validation.CodeRequired, "List every request type.")
	}
	if len(ids) > MaxTypes {
		v.Add("ids", validation.CodeTooLong, "A project has at most 50 request types.")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	after, err := s.repo.Change(ctx, scope.Principal.WorkspaceId, scope.Project.Id, func(cur []Type) (Plan, error) {
		if len(cur) != len(ids) {
			return Plan{}, ErrOrderConflict
		}
		var updates []Update
		for i, id := range ids {
			t, ok := find(cur, id)
			if !ok {
				return Plan{}, ErrOrderConflict
			}
			if want := (i + 1) * orderStep; t.Order != want {
				updates = append(updates, Update{Id: id, Order: &want, Version: t.Version + 1})
			}
		}
		if len(updates) == 0 {
			return Plan{}, nil
		}
		return Plan{Update: updates, Audit: s.event(ctx, scope, audit.RequestTypeReordered, "requestTypes", "order")}, nil
	})
	return after, mapErr(err)
}

// Delete removes a request type with If-Match (admins). It is refused with
// requestTypeInUse while any ticket carries it, and with
// requestTypeLastActive for the only active type.
func (s *Service) Delete(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64) error {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return err
	}
	if !validation.ValidID(id) {
		return errNotFound()
	}
	_, err := s.repo.Change(ctx, scope.Principal.WorkspaceId, scope.Project.Id, func(cur []Type) (Plan, error) {
		t, ok := find(cur, id)
		if !ok {
			return Plan{}, ErrNotFound
		}
		if t.Version != expectedVersion {
			return Plan{}, ErrConflict
		}
		if t.Active && countActive(cur) == 1 {
			return Plan{}, ErrLastActive
		}
		return Plan{Delete: id, Audit: s.event(ctx, scope, audit.RequestTypeDeleted, "requestType/"+id, "name", "code")}, nil
	})
	return mapErr(err)
}

// DeriveCode turns a name into a camelCase code: words are runs of ASCII
// letters and digits, every word is lower-cased, every word after the first
// starts with a capital, leading digits are dropped and the result is cut to
// 40 characters. The result may still be too short or empty; the caller
// checks it against the code pattern.
func DeriveCode(name string) string {
	var words []string
	var cur []byte
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
			cur = append(cur, c+'a'-'A')
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			cur = append(cur, c)
		case c >= 0x80:
			// Non-ASCII characters are dropped without ending the word.
		default:
			flush()
		}
	}
	flush()
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		b.WriteString(w)
	}
	code := strings.TrimLeft(b.String(), "0123456789")
	if len(code) > 40 {
		code = code[:40]
	}
	return code
}
