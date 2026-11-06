// Package customers manages contacts. Contacts are requesters: they never
// sign in. They are project-scoped and are upserted by external ID or
// email, typically by integrations.
package customers

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Contact is a requester in one project.
type Contact struct {
	Id              string
	WorkspaceId     string
	ProjectId       string
	Name            string
	Email           string
	EmailNormalized string
	ExternalId      string
	// OrganisationId is the organisation the integration linked the contact
	// to ("" when none).
	OrganisationId string
	// OrganisationChange is input to UpsertContact only: nil leaves the link
	// unchanged, "" clears it, an ID sets it.
	OrganisationChange *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Version            int64
}

// Repository errors.
var (
	ErrNotFound        = errors.New("customers: not found")
	ErrExternalIDTaken = errors.New("customers: external ID in use")
	ErrContactConflict = errors.New("customers: external ID and email identify different contacts")
	ErrEmailTaken      = errors.New("customers: contact email in use")
	// ErrOrganisationNotFound means the organisation does not exist in the project.
	ErrOrganisationNotFound = errors.New("customers: organisation not found")
	// ErrOrganisationInactive means the organisation was deactivated and cannot be newly linked.
	ErrOrganisationInactive = errors.New("customers: organisation inactive")
)

// UpsertResult reports whether a contact was created.
type UpsertResult struct {
	Contact Contact
	Created bool
}

// Repository persists contacts.
type Repository interface {
	// UpsertContact matches by external ID first, then by email, inside one
	// transaction; created/updated events are filled in by the repository.
	UpsertContact(ctx context.Context, c Contact, created, updated audit.Event) (UpsertResult, error)
	GetContact(ctx context.Context, projectID, id string) (Contact, error)
	// OrganisationExists reports whether the organisation exists in the project.
	OrganisationExists(ctx context.Context, projectID, id string) (bool, error)
	// FindContact matches by external ID first, then normalized email.
	FindContact(ctx context.Context, projectID, externalID, emailNormalized string) (Contact, error)
	ListContacts(ctx context.Context, projectID, prefix string, after *pagination.Cursor, limit int) ([]Contact, error)
}

// Service implements contact management.
type Service struct {
	repo  Repository
	clock clock.Clock
}

// NewService returns a customers service.
func NewService(repo Repository, clk clock.Clock) *Service { return &Service{repo: repo, clock: clk} }

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrExternalIDTaken):
		return fault.Conflict("externalIdTaken", "Another record already uses this external ID.")
	case errors.Is(err, ErrContactConflict):
		return fault.Conflict("contactConflict", "The external ID and the email address belong to different contacts.")
	case errors.Is(err, ErrOrganisationNotFound):
		return fault.Field("organisationId", "notFound", "No organisation with this ID exists in the project.")
	case errors.Is(err, ErrOrganisationInactive):
		return fault.Field("organisationId", "organisationInactive", "This organisation is inactive and cannot be linked to contacts.")
	case errors.Is(err, ErrEmailTaken):
		return fault.Conflict("contactEmailTaken", "Another contact in this project already uses this email address.")
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- Contacts ----

// ContactInput is the upsert-contact command.
type ContactInput struct {
	Name       string
	Email      string
	ExternalId *string
	// OrganisationId links the contact to an organisation (nil unchanged,
	// empty clears).
	OrganisationId *string
}

// Validate checks field rules.
func (in ContactInput) Validate() error {
	var v validation.Collector
	v.String(validation.ContactName, "name", in.Name)
	v.String(validation.ContactEmail, "email", in.Email)
	v.OptionalString(validation.ContactExternalID, "externalId", in.ExternalId)
	if in.OrganisationId != nil && *in.OrganisationId != "" {
		v.String(validation.ObjectID, "organisationId", *in.OrganisationId)
	}
	return v.Err()
}

// UpsertContact creates or updates a project contact, matching an existing
// contact by external ID first and then by email address.
func (s *Service) UpsertContact(ctx context.Context, scope *access.ProjectScope, in ContactInput) (UpsertResult, error) {
	if err := scope.Require(access.CapContactWrite); err != nil {
		return UpsertResult{}, err
	}
	if err := in.Validate(); err != nil {
		return UpsertResult{}, err
	}
	c := Contact{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, Name: in.Name, Email: in.Email,
		EmailNormalized: users.NormalizeEmail(in.Email), ExternalId: deref(in.ExternalId), OrganisationChange: in.OrganisationId}
	now := s.clock.Now()
	c.CreatedAt, c.UpdatedAt = now, now
	created := audit.New(ctx, scope.AuditActor(), audit.ContactCreated, "", now)
	created.ProjectId = scope.Project.Id
	updated := audit.New(ctx, scope.AuditActor(), audit.ContactUpdated, "", now)
	updated.ProjectId = scope.Project.Id
	res, err := s.repo.UpsertContact(ctx, c, created, updated)
	return res, mapErr(err)
}

// GetContact returns one contact.
func (s *Service) GetContact(ctx context.Context, scope *access.ProjectScope, id string) (Contact, error) {
	if err := scope.Require(access.CapContactRead); err != nil {
		return Contact{}, err
	}
	if !validation.ValidID(id) {
		return Contact{}, fault.NotFound("contactNotFound")
	}
	c, err := s.repo.GetContact(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) {
		return Contact{}, fault.NotFound("contactNotFound")
	}
	return c, err
}

// ListContacts pages through contacts ordered by email, optionally filtered
// by an email prefix.
func (s *Service) ListContacts(ctx context.Context, scope *access.ProjectScope, q string, after *pagination.Cursor, limit int) (pagination.Page[Contact], error) {
	if err := scope.Require(access.CapContactRead); err != nil {
		return pagination.Page[Contact]{}, err
	}
	var v validation.Collector
	v.String(validation.SearchQuery, "q", q)
	if err := v.Err(); err != nil {
		return pagination.Page[Contact]{}, err
	}
	rows, err := s.repo.ListContacts(ctx, scope.Project.Id, users.NormalizeEmail(q), after, limit+1)
	if err != nil {
		return pagination.Page[Contact]{}, err
	}
	return pagination.NewPage(rows, limit, func(c Contact) pagination.Cursor {
		return pagination.Cursor{Sort: c.EmailNormalized, Id: c.Id}
	}), nil
}

// ---- Requesters (used by ticket creation) ----

// RequesterByID returns a project contact for a new ticket. Creating a
// ticket (ticket.create) is enough to reference an existing contact.
func (s *Service) RequesterByID(ctx context.Context, scope *access.ProjectScope, id string) (Contact, error) {
	if err := scope.Require(access.CapTicketCreate); err != nil {
		return Contact{}, err
	}
	notFound := fault.Field("requester.contactId", "notFound", "No contact with this ID exists in the project.")
	if !validation.ValidID(id) {
		return Contact{}, notFound
	}
	c, err := s.repo.GetContact(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) {
		return Contact{}, notFound
	}
	return c, err
}

// OrganisationContact returns a project contact for a reply posted on behalf
// of a teammate of the requester. The caller has already been authorised to
// reply as the requester and checks the contact's organisation.
func (s *Service) OrganisationContact(ctx context.Context, scope *access.ProjectScope, id string) (Contact, error) {
	notFound := fault.Field("contactId", "notFound", "No contact with this ID exists in the project.")
	if !validation.ValidID(id) {
		return Contact{}, notFound
	}
	c, err := s.repo.GetContact(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) {
		return Contact{}, notFound
	}
	return c, err
}

// EnsureRequester resolves the requester of a new ticket. Callers with
// contact.write upsert the contact (name changes are applied); callers
// with only ticket.create reuse a matching contact unchanged or create a
// new one.
func (s *Service) EnsureRequester(ctx context.Context, scope *access.ProjectScope, name, email string, externalID, organisationID *string) (Contact, error) {
	if err := scope.Require(access.CapTicketCreate); err != nil {
		return Contact{}, err
	}
	in := ContactInput{Name: name, Email: email, ExternalId: externalID, OrganisationId: organisationID}
	var v validation.Collector
	v.String(validation.ContactName, "requester.name", in.Name)
	v.String(validation.ContactEmail, "requester.email", in.Email)
	v.OptionalString(validation.ContactExternalID, "requester.externalId", in.ExternalId)
	if in.OrganisationId != nil && *in.OrganisationId != "" {
		v.String(validation.ObjectID, "requester.organisationId", *in.OrganisationId)
	}
	if err := v.Err(); err != nil {
		return Contact{}, err
	}
	// The organisation must exist, whichever way the requester is resolved.
	if id := deref(in.OrganisationId); id != "" {
		ok, err := s.repo.OrganisationExists(ctx, scope.Project.Id, id)
		if err != nil {
			return Contact{}, err
		}
		if !ok {
			return Contact{}, mapErr(ErrOrganisationNotFound)
		}
	}
	if !scope.Can(access.CapContactWrite) {
		c, err := s.repo.FindContact(ctx, scope.Project.Id, deref(in.ExternalId), users.NormalizeEmail(in.Email))
		if err == nil {
			// Without contacts:write an existing contact is reused unchanged, so a
			// different organisation cannot be applied: refuse instead of ignoring it.
			if in.OrganisationId != nil && *in.OrganisationId != c.OrganisationId {
				return Contact{}, fault.Forbidden("insufficientScope", "Changing an existing contact's organisation needs the contacts:write scope.")
			}
			return c, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Contact{}, mapErr(err)
		}
	}
	now := s.clock.Now()
	c := Contact{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, Name: in.Name, Email: in.Email,
		EmailNormalized: users.NormalizeEmail(in.Email), ExternalId: deref(in.ExternalId), OrganisationChange: in.OrganisationId, CreatedAt: now, UpdatedAt: now}
	created := audit.New(ctx, scope.AuditActor(), audit.ContactCreated, "", now)
	created.ProjectId, created.Reason = scope.Project.Id, "ticket requester"
	updated := audit.New(ctx, scope.AuditActor(), audit.ContactUpdated, "", now)
	updated.ProjectId, updated.Reason = scope.Project.Id, "ticket requester"
	res, err := s.repo.UpsertContact(ctx, c, created, updated)
	if err != nil {
		return Contact{}, mapErr(err)
	}
	return res.Contact, nil
}
