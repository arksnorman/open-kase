// Package organisations manages the customer organisations an integrating
// system serves (owner decision 2026-10-31). The integration creates an
// organisation, gets its ID, and links that ID to its contacts and tickets;
// open-kase never infers an organisation. Admins and tokens with the
// organisations scopes manage them; every staff member can read them.
package organisations

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Organisation is a customer organisation in one project.
type Organisation struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	Name        string
	// ExternalRef is the integrator's own ID for the organisation ("" when none).
	ExternalRef string
	// Active organisations can be linked to new contacts and tickets; an inactive one
	// keeps its existing links and names.
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int64
	// ContactCount and TicketCount are filled by List, Get and Update for callers who manage
	// organisations.
	ContactCount int64
	TicketCount  int64
}

// NormalizeName is the key of the per-project unique name.
func NormalizeName(name string) string { return strings.ToLower(CleanName(name)) }

// CleanName trims a name and collapses runs of whitespace into one space; it is the stored
// display name and the base of the uniqueness key.
func CleanName(name string) string { return strings.Join(strings.Fields(name), " ") }

// Repository errors.
var (
	ErrNotFound  = errors.New("organisations: not found")
	ErrConflict  = errors.New("organisations: version conflict")
	ErrNameTaken = errors.New("organisations: name exists")
	ErrRefTaken  = errors.New("organisations: external reference exists")
	ErrInUse     = errors.New("organisations: in use")
	ErrInactive  = errors.New("organisations: inactive")
)

// Repository persists organisations.
type Repository interface {
	// List pages through the project's organisations by name; q matches the
	// name or external reference (case-insensitive substring); withUsage fills
	// the counts.
	// active is "true", "false" or "all".
	List(ctx context.Context, projectID, q, active, externalRef string, after *pagination.Cursor, limit int, withUsage bool) ([]Organisation, error)
	Get(ctx context.Context, projectID, id string) (Organisation, error)
	// Usage returns the contact and ticket counts per organisation id.
	Usage(ctx context.Context, projectID string, ids []string) (map[string][2]int64, error)
	Create(ctx context.Context, o Organisation, ev audit.Event) (Organisation, error)
	Update(ctx context.Context, o Organisation, expectedVersion int64, ev audit.Event) (Organisation, error)
	// Delete refuses with ErrInUse while a contact or ticket references it.
	Delete(ctx context.Context, o Organisation, expectedVersion int64, ev audit.Event) error
}

// Service implements organisation management.
type Service struct {
	repo  Repository
	clock clock.Clock
}

// NewService returns the organisations service.
func NewService(repo Repository, clk clock.Clock) *Service { return &Service{repo: repo, clock: clk} }

func errNotFound() error { return fault.NotFound("organisationNotFound") }

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return errNotFound()
	case errors.Is(err, ErrConflict):
		return fault.Conflict("versionConflict", "The organisation was changed by someone else. Reload and try again.")
	case errors.Is(err, ErrNameTaken):
		return fault.Conflict("organisationExists", "An organisation with this name already exists.")
	case errors.Is(err, ErrRefTaken):
		return fault.Conflict("organisationRefTaken", "Another organisation already uses this external reference.")
	case errors.Is(err, ErrInUse):
		return fault.Conflict("organisationInUse", "Contacts or tickets still reference this organisation. Deactivate it instead, or unlink them first.")
	}
	return err
}

// Input creates or changes an organisation. A nil field is unchanged; an
// empty ExternalRef clears it.
type Input struct {
	Name        *string
	ExternalRef *string
	// Active deactivates (false) or reactivates (true) the organisation; nil is unchanged.
	Active *bool
}

func validate(o Organisation) error {
	var v validation.Collector
	v.String(validation.OrganisationName, "name", o.Name)
	v.String(validation.OrganisationExternalRef, "externalRef", o.ExternalRef)
	return v.Err()
}

// List pages through organisations ordered by name (staff users and tokens
// with organisations:read or organisations:write). Counts are reported to
// callers who manage organisations.
func (s *Service) List(ctx context.Context, scope *access.ProjectScope, q, active, externalRef string, after *pagination.Cursor, limit int) (pagination.Page[Organisation], error) {
	if err := scope.Require(access.CapOrganisationRead); err != nil {
		return pagination.Page[Organisation]{}, err
	}
	if active == "" {
		active = "true"
	}
	var v validation.Collector
	v.String(validation.SearchQuery, "q", q)
	v.String(validation.OrganisationActiveFilter, "active", active)
	if externalRef != "" {
		v.String(validation.OrganisationExternalRef, "externalRef", externalRef)
	}
	if err := v.Err(); err != nil {
		return pagination.Page[Organisation]{}, err
	}
	rows, err := s.repo.List(ctx, scope.Project.Id, strings.TrimSpace(q), active, externalRef, after, limit+1, scope.Can(access.CapOrganisationManage))
	if err != nil {
		return pagination.Page[Organisation]{}, err
	}
	return pagination.NewPage(rows, limit, func(o Organisation) pagination.Cursor {
		return pagination.Cursor{Sort: NormalizeName(o.Name), Id: o.Id}
	}), nil
}

// Get returns one organisation.
func (s *Service) Get(ctx context.Context, scope *access.ProjectScope, id string) (Organisation, error) {
	if err := scope.Require(access.CapOrganisationRead); err != nil {
		return Organisation{}, err
	}
	if !validation.ValidID(id) {
		return Organisation{}, errNotFound()
	}
	o, err := s.repo.Get(ctx, scope.Project.Id, id)
	if err != nil {
		return Organisation{}, mapErr(err)
	}
	return s.withUsage(ctx, scope, o)
}

// withUsage fills the counts for callers who manage organisations (the same
// rule List applies).
func (s *Service) withUsage(ctx context.Context, scope *access.ProjectScope, o Organisation) (Organisation, error) {
	if !scope.Can(access.CapOrganisationManage) {
		return o, nil
	}
	counts, err := s.repo.Usage(ctx, scope.Project.Id, []string{o.Id})
	if err != nil {
		return Organisation{}, err
	}
	o.ContactCount, o.TicketCount = counts[o.Id][0], counts[o.Id][1]
	return o, nil
}

// Create adds an organisation (admins; tokens with organisations:write).
func (s *Service) Create(ctx context.Context, scope *access.ProjectScope, in Input) (Organisation, error) {
	if err := scope.Require(access.CapOrganisationManage); err != nil {
		return Organisation{}, err
	}
	now := s.clock.Now()
	o := Organisation{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, Active: true, CreatedAt: now, UpdatedAt: now, Version: 1}
	if in.Name != nil {
		o.Name = CleanName(*in.Name)
	}
	if in.ExternalRef != nil {
		o.ExternalRef = strings.TrimSpace(*in.ExternalRef)
	}
	if err := validate(o); err != nil {
		return Organisation{}, err
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.OrganisationCreated, "", now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name", "externalRef"}
	out, err := s.repo.Create(ctx, o, ev)
	return out, mapErr(err)
}

// Update changes an organisation with If-Match.
func (s *Service) Update(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64, in Input) (Organisation, error) {
	if err := scope.Require(access.CapOrganisationManage); err != nil {
		return Organisation{}, err
	}
	o, err := s.Get(ctx, scope, id)
	if err != nil {
		return Organisation{}, err
	}
	var changed []string
	if in.Name != nil && CleanName(*in.Name) != o.Name {
		o.Name, changed = CleanName(*in.Name), append(changed, "name")
	}
	if in.ExternalRef != nil && strings.TrimSpace(*in.ExternalRef) != o.ExternalRef {
		o.ExternalRef, changed = strings.TrimSpace(*in.ExternalRef), append(changed, "externalRef")
	}
	if in.Active != nil && *in.Active != o.Active {
		o.Active, changed = *in.Active, append(changed, "active")
	}
	if err := validate(o); err != nil {
		return Organisation{}, err
	}
	if o.Version != expectedVersion {
		return Organisation{}, mapErr(ErrConflict)
	}
	if len(changed) == 0 {
		return o, nil
	}
	now := s.clock.Now()
	o.UpdatedAt = now
	ev := audit.New(ctx, scope.AuditActor(), audit.OrganisationUpdated, "organisation/"+o.Id, now)
	ev.ProjectId, ev.Changes = scope.Project.Id, changed
	out, err := s.repo.Update(ctx, o, expectedVersion, ev)
	if err != nil {
		return Organisation{}, mapErr(err)
	}
	return s.withUsage(ctx, scope, out)
}

// Delete removes an organisation with If-Match; it is refused with
// organisationInUse while any contact or ticket references it.
func (s *Service) Delete(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64) error {
	if err := scope.Require(access.CapOrganisationManage); err != nil {
		return err
	}
	o, err := s.Get(ctx, scope, id)
	if err != nil {
		return err
	}
	if o.Version != expectedVersion {
		return mapErr(ErrConflict)
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.OrganisationDeleted, "organisation/"+o.Id, s.clock.Now())
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name"}
	return mapErr(s.repo.Delete(ctx, o, expectedVersion, ev))
}
