package tickets

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Limits is the narrow settings view for message and attachment sizes.
type Limits struct {
	MaxAttachmentBytes       int64
	MaxAttachmentsPerMessage int64
	MaxMessageTotalBytes     int64
	MaxMessageCodePoints     int64
}

// Requesters resolves ticket requesters (implemented by customers.Service).
type Requesters interface {
	RequesterByID(ctx context.Context, scope *access.ProjectScope, id string) (customers.Contact, error)
	// OrganisationContact returns a project contact (contactNotInOrganisation
	// style 422 when it does not exist); the caller checks its organisation.
	OrganisationContact(ctx context.Context, scope *access.ProjectScope, id string) (customers.Contact, error)
	EnsureRequester(ctx context.Context, scope *access.ProjectScope, name, email string, externalID, organisationID *string) (customers.Contact, error)
}

// Deps are the service's collaborators.
type Deps struct {
	Repo       Repository
	Files      FileStore
	Catalog    statuses.CatalogRepository
	Requesters Requesters
	Clock      clock.Clock
	Limits     func() Limits
	// SLA is optional (nil: SLA off).
	SLA SLAPolicies
	// Jobs, Wake and Authors enable scheduled messages (nil Jobs: 503
	// schedulingUnavailable). Wake shortens the job polling delay.
	Jobs    jobs.Store
	Wake    func(ctx context.Context)
	Authors AuthorScopes
	// Signatures is optional (nil: no signatures are appended).
	Signatures Signatures
}

// Service implements ticket, message, draft and attachment commands.
type Service struct {
	repo       Repository
	files      FileStore
	catalog    statuses.CatalogRepository
	requesters Requesters
	clock      clock.Clock
	limits     func() Limits
	sla        SLAPolicies
	jobs       jobs.Store
	wakeJobs   func(ctx context.Context)
	authors    AuthorScopes
	signatures Signatures
}

// NewService returns a tickets service.
func NewService(d Deps) *Service {
	return &Service{repo: d.Repo, files: d.Files, catalog: d.Catalog, requesters: d.Requesters, clock: d.Clock, limits: d.Limits,
		sla: d.SLA, jobs: d.Jobs, wakeJobs: d.Wake, authors: d.Authors, signatures: d.Signatures}
}

// Stable errors.
func errTicketNotFound() error { return fault.NotFound("ticketNotFound") }

func errArchived() error {
	return fault.Conflict("ticketArchived", "This ticket is archived and cannot change. Create a linked follow-up ticket instead.")
}

func errVersion() error {
	return fault.Conflict("versionConflict", "This ticket was changed by someone else. Reload and try again.")
}

func errAlreadyClaimed() error {
	return fault.Conflict("ticketAlreadyClaimed", "Another agent claimed this ticket first. Reload to see the current owner.")
}

func mapLifecycle(err error) error {
	switch {
	case errors.Is(err, ErrArchived):
		return errArchived()
	case errors.Is(err, ErrInvalidTransition):
		return fault.Conflict("invalidTransition", "This action is not allowed from the ticket's current status.")
	case errors.Is(err, ErrStatusNotAllowed):
		return fault.Field("statusId", "notAllowed", "Choose an open or awaiting-customer status; use hold, resolve or archive for those states.")
	}
	return err
}

// FollowUpAction is the "Create follow-up ticket" action offered with
// ticketArchived problems.
func FollowUpAction(number int64) fault.Action {
	return fault.Action{Rel: "createFollowUp", Title: "Create follow-up ticket", Method: "POST",
		Href: "/api/v1/tickets/" + strconv.FormatInt(number, 10) + "/follow-up"}
}

// archivedErr is ticketArchived with the follow-up action for t.
func archivedErr(scope *access.ProjectScope, t Ticket) error {
	e := fault.Conflict("ticketArchived", "This ticket is archived and cannot change. Create a linked follow-up ticket instead.")
	if scope == nil || t.Number == 0 {
		return e
	}
	return e.WithActions(FollowUpAction(t.Number))
}

// lifecycleErr maps a lifecycle planning error, attaching the follow-up
// action to ticketArchived.
func lifecycleErr(scope *access.ProjectScope, t Ticket, err error) error {
	if errors.Is(err, ErrArchived) {
		return archivedErr(scope, t)
	}
	return mapLifecycle(err)
}

func (s *Service) statusSet(ctx context.Context, scope *access.ProjectScope) (StatusSet, error) {
	all, err := s.catalog.ProjectStatuses(ctx, scope.Principal.WorkspaceId, scope.Project.Id)
	if err != nil {
		return StatusSet{}, err
	}
	return NewStatusSet(all)
}

// ---- Access ----

// readAccess returns the ticket visibility of the scope. API tokens see
// every ticket (requester tokens public content only); administrators see all groups; agents see their groups' tickets and the
// open, active or waiting tickets assigned to them (their own work in a
// group that was deactivated). A resolved or archived ticket is never
// readable through a leftover assignment alone: a member removed from a
// group loses it with the group (the release job clears their workable
// assignments; resolved and archived ones keep the historical assignee).
func readAccess(scope *access.ProjectScope) (Access, error) {
	if err := scope.Require(access.CapTicketRead); err != nil {
		return Access{}, err
	}
	if scope.Principal.Kind == access.KindIntegration || scope.Can(access.CapGroupReadAll) {
		return Access{All: true}, nil
	}
	return Access{GroupIds: slices.Clone(scope.GroupIds), UserId: scope.Principal.Id}, nil
}

func (a Access) allows(t Ticket) bool {
	return a.All || slices.Contains(a.GroupIds, t.Group.Id) ||
		(a.UserId != "" && t.AssigneeID() == a.UserId && t.StatusCategory.Workable())
}

func viewerID(scope *access.ProjectScope) string {
	if scope.Principal.IsUser() {
		return scope.Principal.Id
	}
	return ""
}

// numberRef matches a ticket number with an optional "#": "1042", "#1042".
var numberRef = regexp.MustCompile(`^#?([1-9][0-9]{0,17})$`)

// ParseRef reads a ticket reference: an ObjectID or a number ("1042" or
// "#1042"). It returns the ID, or the number
// when the reference is not an ID; ok is false for anything else.
func ParseRef(ref string) (id string, number int64, ok bool) {
	ref = strings.TrimSpace(ref)
	if validation.ValidID(ref) {
		return ref, 0, true
	}
	m := numberRef.FindStringSubmatch(ref)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	return "", n, err == nil
}

// load returns an accessible ticket or 404 ticketNotFound.
func (s *Service) load(ctx context.Context, scope *access.ProjectScope, ref string) (Ticket, error) {
	acc, err := readAccess(scope)
	if err != nil {
		return Ticket{}, err
	}
	id, number, ok := ParseRef(ref)
	if !ok {
		return Ticket{}, errTicketNotFound()
	}
	var t Ticket
	if id != "" {
		t, err = s.repo.Get(ctx, scope.Project.Id, id, viewerID(scope))
	} else {
		t, err = s.repo.GetByNumber(ctx, scope.Project.Id, number, viewerID(scope))
	}
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, errTicketNotFound()
	}
	if err != nil {
		return Ticket{}, err
	}
	if !acc.allows(t) {
		return Ticket{}, errTicketNotFound()
	}
	return t, nil
}

// present applies the audience projection: requester tokens act for the
// requester and never see staff identities, staff activity or staff working data (tags, SLA). For staff (users and agent
// tokens) it evaluates the SLA cycle at the current time.
func (s *Service) present(scope *access.ProjectScope, t Ticket) Ticket {
	if scope.Principal.IsStaff() {
		if t.SLA != nil {
			t.SLAView = t.SLA.Summarize(s.clock.Now())
		}
		return t
	}
	t.Tags, t.SLA, t.SLAView, t.FirstResponseAt = nil, nil, nil, nil
	t.Assignee, t.Hold = nil, nil
	t.WatcherCount, t.Watching = 0, false
	t.StaffActivityAt = t.PublicActivityAt
	t.UpdatedAt = t.PublicUpdatedAt
	// The version moves with every staff change; integrations cannot send
	// If-Match on tickets, so they receive 0 instead of a staff signal.
	t.Version = 0
	return t
}

func (s *Service) actor(scope *access.ProjectScope) audit.Actor { return scope.AuditActor() }

// staffAuthor is the message author for a staff principal: the user, or the
// agent token as a bot under its own name.
func staffAuthor(p access.Principal) Author {
	if p.IsUser() {
		return Author{Kind: AuthorUser, Id: p.Id, Name: p.Name}
	}
	return Author{Kind: AuthorIntegration, Id: p.Id, Name: p.Name}
}

func (s *Service) event(ctx context.Context, scope *access.ProjectScope, t Ticket, action audit.Action, now time.Time, changes ...string) audit.Event {
	ev := audit.New(ctx, s.actor(scope), action, "ticket/"+t.Id, now)
	ev.ProjectId, ev.TicketId, ev.Changes = scope.Project.Id, t.Id, changes
	return ev
}

func (s *Service) outboxEvent(scope *access.ProjectScope, typ, ticketID, visibility string, now time.Time) outbox.Event {
	return outbox.Event{Type: typ, ProjectId: scope.Project.Id, TicketId: ticketID, Visibility: visibility, Actor: s.actor(scope), OccurredAt: now}
}

func (s *Service) withChanges(e outbox.Event, changes []string) outbox.Event {
	e.Changes = slices.Clone(changes)
	return e
}

// ---- Create ----

// RequesterInput identifies the requester: an existing contact by ID, or a
// contact upserted from name, email and optional external ID.
type RequesterInput struct {
	ContactId  *string
	Name       *string
	Email      *string
	ExternalId *string
	// OrganisationId links a new or updated requester contact to an
	// organisation (explicit only; never inferred).
	OrganisationId *string
}

// CreateInput is the create-ticket command.
type CreateInput struct {
	Subject       string
	Body          string
	Requester     RequesterInput
	Priority      *int
	RequestTypeId *string
	GroupId       *string
	AttachmentIds []string
	// OrganisationId is the organisation the creator links the ticket to
	// (optional; explicit, never inferred from the requester).
	OrganisationId *string
	// keepOrganisation lets a copy or follow-up keep the original's organisation even
	// when it has been deactivated since.
	keepOrganisation bool
	// CCEmails are optional addresses copied on requester emails.
	CCEmails []string
	// Tags are catalog tags for the new ticket (copying only; staff).
	Tags []string
}

// validateOrg checks the optional organisation ID (empty clears on update).
func validateOrg(v *validation.Collector, id *string) {
	if id != nil && *id != "" {
		v.String(validation.ObjectID, "organisationId", *id)
	}
}

func (s *Service) bodyRule(v *validation.Collector, field, body string) {
	v.String(validation.MessageBody, field, body)
	if max := s.limits().MaxMessageCodePoints; max > 0 && int64(utf8.RuneCountInString(body)) > max {
		v.Add(field, validation.CodeTooLong, "The message is longer than the configured limit of "+strconv.FormatInt(max, 10)+" characters.")
	}
}

func validateIDs(v *validation.Collector, field string, ids []string) {
	for i, id := range ids {
		v.String(validation.ObjectID, field+"["+strconv.Itoa(i)+"]", id)
	}
}

// Validate checks the command's shape.
func (in CreateInput) validate(s *Service) error {
	var v validation.Collector
	v.String(validation.TicketSubject, "subject", in.Subject)
	s.bodyRule(&v, "body", in.Body)
	if in.Priority != nil {
		v.Int(validation.TicketPriority, "priority", int64(*in.Priority))
	}
	if in.RequestTypeId != nil {
		v.String(validation.ObjectID, "requestTypeId", *in.RequestTypeId)
	}
	if in.GroupId != nil {
		v.String(validation.ObjectID, "groupId", *in.GroupId)
	}
	validateIDs(&v, "attachmentIds", in.AttachmentIds)
	validateOrg(&v, in.OrganisationId)
	NormalizeCCEmails(&v, "ccEmails", in.CCEmails, deref(in.Requester.Email))
	r := in.Requester
	switch {
	case r.ContactId != nil:
		if r.Name != nil || r.Email != nil || r.ExternalId != nil || r.OrganisationId != nil {
			v.Add("requester", "invalidCombination", "Send either contactId or name and email, not both.")
		}
		v.String(validation.ObjectID, "requester.contactId", *r.ContactId)
	case r.Name == nil && r.Email == nil:
		v.Add("requester", validation.CodeRequired, "Identify the requester by contactId or by name and email.")
	default:
		if r.Name == nil {
			v.Add("requester.name", validation.CodeRequired, "This field is required.")
		}
		if r.Email == nil {
			v.Add("requester.email", validation.CodeRequired, "This field is required.")
		}
	}
	return v.Err()
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Create opens a ticket in the requested active group or Triage, never
// assigned to a person, with its first public message. Agents create
// tickets from the web and agent tokens through the API (the agent or bot
// is the first message's author); requester tokens create them as the
// requester.
func (s *Service) Create(ctx context.Context, scope *access.ProjectScope, in CreateInput) (Ticket, error) {
	return s.create(ctx, scope, in, nil)
}

func (s *Service) create(ctx context.Context, scope *access.ProjectScope, in CreateInput, fu *followUp) (Ticket, error) {
	if err := scope.Require(access.CapTicketCreate); err != nil {
		return Ticket{}, err
	}
	if err := in.validate(s); err != nil {
		return Ticket{}, err
	}
	// A retry of a create that already committed returns that ticket, even
	// though its attachments are no longer staged.
	if t, done, err := s.replayed(ctx, scope, "ticket/"); done || err != nil {
		if err != nil {
			return Ticket{}, err
		}
		out, err := s.repo.Get(ctx, scope.Project.Id, t.Id, viewerID(scope))
		if errors.Is(err, ErrNotFound) {
			return Ticket{}, errTicketNotFound()
		}
		if err != nil {
			return Ticket{}, err
		}
		return s.present(scope, out), nil
	}
	p := scope.Principal

	var group Ref
	if in.GroupId != nil {
		g, active, err := s.repo.ActiveGroup(ctx, scope.Project.Id, *in.GroupId)
		if errors.Is(err, ErrNotFound) {
			return Ticket{}, fault.Field("groupId", "notFound", "No group with this ID exists in the project.")
		}
		if err != nil {
			return Ticket{}, err
		}
		if !active {
			return Ticket{}, fault.Field("groupId", "inactive", "The group is inactive.")
		}
		group = g
	} else {
		g, err := s.repo.DefaultGroup(ctx, scope.Project.Id)
		if err != nil {
			return Ticket{}, err
		}
		group = g
	}

	rt, err := s.requestType(ctx, scope, in.RequestTypeId)
	if err != nil {
		return Ticket{}, err
	}
	set, err := s.statusSet(ctx, scope)
	if err != nil {
		return Ticket{}, err
	}
	uploader := Uploader{Kind: string(p.Kind), Id: p.Id}
	attIDs, err := s.checkAttachments(ctx, scope, uploader, in.AttachmentIds)
	if err != nil {
		return Ticket{}, err
	}

	var contact customers.Contact
	if in.Requester.ContactId != nil {
		contact, err = s.requesters.RequesterByID(ctx, scope, *in.Requester.ContactId)
	} else {
		contact, err = s.requesters.EnsureRequester(ctx, scope, deref(in.Requester.Name), deref(in.Requester.Email), in.Requester.ExternalId, in.Requester.OrganisationId)
	}
	if err != nil {
		return Ticket{}, err
	}

	var org *OrgRef
	if id := deref(in.OrganisationId); id != "" {
		if org, err = s.organisation(ctx, scope, id, in.keepOrganisation); err != nil {
			return Ticket{}, err
		}
	}

	// The requester is known only now (contactId): check CC against it.
	var cv validation.Collector
	cc := NormalizeCCEmails(&cv, "ccEmails", in.CCEmails, contact.Email)
	if err := cv.Err(); err != nil {
		return Ticket{}, err
	}

	now := s.clock.Now()
	t := Ticket{
		WorkspaceId: p.WorkspaceId, ProjectId: scope.Project.Id, Subject: in.Subject,
		CCEmails: cc, Organisation: org,
		Requester: ContactRef{Id: contact.Id, Name: contact.Name, Email: contact.Email, ExternalId: contact.ExternalId},
		Group:     group, Priority: DefaultPriority, Status: Ref{Id: set.Open.Id, Name: set.Open.Name},
		StatusCategory: statuses.CategoryOpen, RequestType: Ref{Id: rt.Id, Name: rt.Name},
		CreatedAt: now, UpdatedAt: now, PublicUpdatedAt: now, PublicActivityAt: now, StaffActivityAt: now, Version: 1,
	}
	msg := Message{ProjectId: scope.Project.Id, Visibility: VisibilityPublic, Body: in.Body, CreatedAt: now}
	if len(in.Tags) > 0 {
		var tv validation.Collector
		tags := NormalizeTags(&tv, "tags", in.Tags)
		if err := tv.Err(); err != nil {
			return Ticket{}, err
		}
		if err := s.checkTagsKnown(ctx, scope.Project.Id, in.Tags, tags, nil); err != nil {
			return Ticket{}, err
		}
		t.Tags = tags
	}
	if fu != nil && fu.signed {
		if msg.Body, err = s.signed(ctx, p, msg.Body); err != nil {
			return Ticket{}, err
		}
		if err = s.checkSigned(in.Body, msg.Body); err != nil {
			return Ticket{}, err
		}
	}
	if p.IsStaff() {
		t.Source = SourceWeb
		if !p.IsUser() {
			t.Source = SourceAPI
		}
		if in.Priority != nil {
			t.Priority = *in.Priority
		}
		msg.Author = staffAuthor(p)
	} else {
		// The requester side may request a priority; the effective priority
		// is set by support staff (architecture §3.1).
		t.Source = SourceAPI
		if in.Priority != nil {
			t.RequestedPriority = *in.Priority
		}
		msg.Author = Author{Kind: AuthorContact, Id: contact.Id, Name: contact.Name, ViaId: p.Id}
	}

	ev := audit.New(ctx, s.actor(scope), audit.TicketCreated, "", now)
	ev.ProjectId = scope.Project.Id
	ev.Changes = []string{"subject", "requester", "group", "priority", "requestType", "status"}
	if t.Organisation != nil {
		ev.Changes = append(ev.Changes, "organisationId")
	}
	if len(cc) > 0 {
		ev.Changes = append(ev.Changes, "ccEmails")
	}
	if len(t.Tags) > 0 {
		ev.Changes = append(ev.Changes, "tags")
	}
	if len(attIDs) > 0 {
		ev.Changes = append(ev.Changes, "attachments")
	}
	cmd := CreateCommand{
		Ticket: t, CreatedBy: s.actor(scope), Message: msg,
		AttachmentIds: attIDs, Uploader: uploader, Audit: []audit.Event{ev}, KeepOrganisation: in.keepOrganisation,
		Outbox:   []outbox.Event{s.outboxEvent(scope, outbox.TicketCreated, "", VisibilityPublic, now)},
		ViewerId: viewerID(scope),
	}
	if ref, ok := idempotency.RefFrom(ctx); ok {
		cmd.Idempotency = &ref
	}
	if fu != nil {
		cmd.Link, cmd.LinkAudit, cmd.LinkOutbox = fu.link, fu.audit, fu.outbox
		cmd.Audit[0].Changes = append(cmd.Audit[0].Changes, "links:"+fu.link.Type)
		if fu.copied {
			cmd.Audit = append(cmd.Audit, audit.New(ctx, s.actor(scope), audit.TicketCopied, "", now))
			cmd.Audit[1].ProjectId, cmd.Audit[1].Changes = scope.Project.Id, []string{"links:" + LinkCopy}
		}
	}
	snap, err := s.slaSnapshot(ctx, scope.Project.Id)
	if err != nil {
		return Ticket{}, err
	}
	if snap != nil {
		in := sla.Start(*snap, t.Priority, 1, now, sla.StartOptions{Quiet: outbox.Suppressed(ctx)})
		in.WorkspaceId, in.ProjectId = p.WorkspaceId, scope.Project.Id
		cmd.SLA = &in
	}
	out, err := s.repo.Create(ctx, cmd)
	switch {
	case errors.Is(err, ErrAttachmentsGone):
		return Ticket{}, errAttachmentsGone()
	case errors.Is(err, ErrGroupUnavailable):
		return Ticket{}, errGroupInactive()
	case errors.Is(err, ErrNotFound):
		return Ticket{}, errTicketNotFound()
	case errors.Is(err, ErrTooManyLinks):
		// A follow-up of a ticket that already has 100 links.
		return Ticket{}, errTooManyLinks()
	case err != nil:
		return Ticket{}, err
	}
	return s.present(scope, out), nil
}

func errGroupInactive() error {
	return fault.Field("groupId", "inactive", "The group is inactive or was removed.")
}

// replayed looks up the resource a committed command recorded on this
// request's idempotency reservation. done is true when one exists; the
// returned ticket carries only the resource ID (ticket or message).
func (s *Service) replayed(ctx context.Context, scope *access.ProjectScope, prefix string) (Ticket, bool, error) {
	ref, ok := idempotency.RefFrom(ctx)
	if !ok {
		return Ticket{}, false, nil
	}
	res, err := s.repo.IdempotentResource(ctx, ref)
	if err != nil {
		return Ticket{}, false, err
	}
	id, found := strings.CutPrefix(res, prefix)
	if !found {
		return Ticket{}, false, nil
	}
	return Ticket{Id: id}, true, nil
}

// organisation resolves the organisation a staff member or integration
// links to a ticket; it must exist in the project.
func (s *Service) organisation(ctx context.Context, scope *access.ProjectScope, id string, allowInactive bool) (*OrgRef, error) {
	o, err := s.repo.Organisation(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) {
		return nil, fault.Field("organisationId", "notFound", "No organisation with this ID exists in the project.")
	}
	if err != nil {
		return nil, err
	}
	if !o.Active && !allowInactive {
		return nil, fault.Field("organisationId", "organisationInactive", "This organisation is inactive and cannot be linked to new tickets.")
	}
	return &o, nil
}

func (s *Service) checkOrganisation(ctx context.Context, scope *access.ProjectScope, id string) error {
	_, err := s.organisation(ctx, scope, id, false)
	return err
}

func (s *Service) requestType(ctx context.Context, scope *access.ProjectScope, id *string) (statuses.RequestType, error) {
	all, err := s.catalog.ProjectRequestTypes(ctx, scope.Principal.WorkspaceId, scope.Project.Id)
	if err != nil {
		return statuses.RequestType{}, err
	}
	for _, rt := range all {
		if (id != nil && rt.Id == *id) || (id == nil && rt.Code == "issue") {
			return rt, nil
		}
	}
	if id == nil && len(all) > 0 {
		return all[0], nil
	}
	return statuses.RequestType{}, fault.Field("requestTypeId", "notFound", "No request type with this ID exists in the project.")
}

// ---- Queries ----

// ListInput is a ticket list request.
type ListInput struct {
	View    string
	Q       string
	GroupId string
	After   *pagination.Cursor
	Limit   int
	// Filter narrows any view (list parameters; staff only).
	Filter *Filter
}

// Views that only make sense for a signed-in user.
var userViews = []string{"myWork", "unassigned", "watching", "dueSoon"}

var digits = regexp.MustCompile(`^[0-9]{1,18}$`)

func parseSearch(q string) Search {
	q = strings.TrimSpace(q)
	if q == "" {
		return Search{}
	}
	if digits.MatchString(q) {
		n, _ := strconv.ParseInt(q, 10, 64)
		return Search{Number: n, Subject: q}
	}
	if _, n, ok := ParseRef(q); ok && n > 0 {
		return Search{Number: n}
	}
	return Search{Subject: q}
}

// query builds an authorized query for a view. ok is false when the view
// is known to be empty (no SLA data yet, or no groups).
func (s *Service) query(scope *access.ProjectScope, in ListInput) (Query, bool, error) {
	acc, err := readAccess(scope)
	if err != nil {
		return Query{}, false, err
	}
	if in.View == "" {
		in.View = "all"
	}
	var v validation.Collector
	v.String(validation.TicketView, "view", in.View)
	v.String(validation.SearchQuery, "q", in.Q)
	if in.GroupId != "" {
		v.String(validation.ObjectID, "groupId", in.GroupId)
	}
	if err := v.Err(); err != nil {
		return Query{}, false, err
	}
	if slices.Contains(userViews, in.View) {
		if err := scope.Principal.RequireUser(); err != nil {
			return Query{}, false, err
		}
	}
	q := Query{ProjectId: scope.Project.Id, ViewerId: viewerID(scope), Access: acc, GroupFilter: in.GroupId,
		PublicSort: !scope.Principal.IsStaff(),
		Search:     parseSearch(in.Q), After: in.After, Limit: in.Limit}
	me := scope.Principal.Id
	switch in.View {
	case "myWork":
		q.AssigneeId = me
		q.Categories = queueCategories()
	case "unassigned":
		if len(scope.GroupIds) == 0 {
			return q, false, nil
		}
		q.Unassigned, q.GroupIds = true, slices.Clone(scope.GroupIds)
		q.Categories = queueCategories()
	case "watching":
		q.WatchedBy = me
	case "awaitingCustomer":
		q.Categories = []statuses.Category{statuses.CategoryWaitingCustomer}
	case "onHold":
		q.Categories = []statuses.Category{statuses.CategoryOnHold}
	case "dueSoon":
		// Running SLA targets whose warning time passed (due soon or
		// already breached).
		now := s.clock.Now()
		q.WarnBefore = &now
		q.Categories = queueCategories()
	case "resolved":
		q.Categories = []statuses.Category{statuses.CategoryResolved}
	case "archived":
		q.Categories = []statuses.Category{statuses.CategoryArchived}
	}
	if in.Filter != nil {
		if err := scope.Principal.RequireStaff(); err != nil {
			return Query{}, false, err
		}
		if f := in.Filter; f.Assignee != "" && (in.View == "myWork" || in.View == "unassigned") {
			return Query{}, false, fault.Field("assignee", "notAllowed", "This view already fixes the assignee.")
		}
		f := *in.Filter
		var v validation.Collector
		f.Normalize(&v)
		if err := v.Err(); err != nil {
			return Query{}, false, err
		}
		f.apply(&q, me, s.clock.Now())
		if q.None {
			return q, false, nil
		}
	}
	return q, true, nil
}

// queueCategories are the categories of the working queues (assigned to me,
// unassigned, due soon): a ticket on hold stays out of them until it is back.
func queueCategories() []statuses.Category {
	return []statuses.Category{statuses.CategoryOpen, statuses.CategoryWaitingCustomer}
}

func workableCategories() []statuses.Category {
	return []statuses.Category{statuses.CategoryOpen, statuses.CategoryWaitingCustomer, statuses.CategoryOnHold}
}

// List pages through the tickets of a view, newest activity first. Access
// filters are part of the database query, so limits and cursors apply only
// to tickets the caller may see.
func (s *Service) List(ctx context.Context, scope *access.ProjectScope, in ListInput) (pagination.Page[Ticket], error) {
	q, ok, err := s.query(scope, in)
	if err != nil || !ok {
		return pagination.Page[Ticket]{Items: []Ticket{}}, err
	}
	q.Limit = in.Limit + 1
	rows, err := s.repo.List(ctx, q)
	if err != nil {
		return pagination.Page[Ticket]{}, err
	}
	for i := range rows {
		rows[i] = s.present(scope, rows[i])
	}
	// For integrations UpdatedAt already holds the public update time.
	return pagination.NewPage(rows, in.Limit, func(t Ticket) pagination.Cursor {
		return pagination.Cursor{Sort: t.UpdatedAt.UTC().Format(time.RFC3339Nano), Id: t.Id}
	}), nil
}

// QueueCounts are the sidebar counts for the signed-in user.
type QueueCounts struct {
	MyWork, Unassigned, Watching, AwaitingCustomer, OnHold, DueSoon int64
}

// QueueCounts counts the user's queues with the same authorized filters as
// List.
func (s *Service) QueueCounts(ctx context.Context, scope *access.ProjectScope) (QueueCounts, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return QueueCounts{}, err
	}
	var out QueueCounts
	for view, dst := range map[string]*int64{"myWork": &out.MyWork, "unassigned": &out.Unassigned, "watching": &out.Watching, "awaitingCustomer": &out.AwaitingCustomer, "onHold": &out.OnHold, "dueSoon": &out.DueSoon} {
		q, ok, err := s.query(scope, ListInput{View: view})
		if err != nil {
			return QueueCounts{}, err
		}
		if !ok {
			continue
		}
		n, err := s.repo.Count(ctx, q)
		if err != nil {
			return QueueCounts{}, err
		}
		*dst = n
	}
	return out, nil
}

// Get returns one accessible ticket by ID or key.
func (s *Service) Get(ctx context.Context, scope *access.ProjectScope, ref string) (Ticket, error) {
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	return s.present(scope, t), nil
}

// ---- Updates ----

// UpdateInput is the allowlisted ticket PATCH.
type UpdateInput struct {
	Subject       *string
	Priority      *int
	RequestTypeId *string
	// Tags replaces the tag set (normalized); nil leaves it unchanged.
	Tags *[]string
	// OrganisationId sets the ticket's organisation (staff only; "" clears).
	OrganisationId *string
	// CCEmails replaces the CC addresses (staff only; normalized); nil
	// leaves them unchanged.
	CCEmails *[]string
}

// errSubjectLocked refuses subject changes by anyone but the creating
// requester token (owner decision 2026-10-02).
func errSubjectLocked() error {
	return fault.Forbidden("subjectLocked", "The subject cannot be changed after the ticket is created. Only the integration that created the ticket can change it.")
}

// Update edits priority, request type, tags, organisation or CC addresses (staff: users and agent
// tokens with tickets:write) with an expected version. The subject is
// locked after creation: staff receive 403 subjectLocked; only the
// requester token that created the ticket may change it, and a requester
// token may change nothing else. Requester tokens cannot see ticket
// versions, so their change applies to the current version.
func (s *Service) Update(ctx context.Context, scope *access.ProjectScope, ref string, expectedVersion int64, in UpdateInput) (Ticket, error) {
	integration := !scope.Principal.IsStaff()
	if integration {
		if in.Subject == nil || in.Priority != nil || in.RequestTypeId != nil || in.Tags != nil || in.OrganisationId != nil || in.CCEmails != nil {
			return Ticket{}, scope.Principal.RequireStaff()
		}
		if err := scope.Require(access.CapTicketCreate); err != nil {
			return Ticket{}, err
		}
	} else {
		if in.Subject != nil {
			return Ticket{}, errSubjectLocked()
		}
		if err := scope.Require(access.CapTicketTransition); err != nil {
			return Ticket{}, err
		}
	}
	var v validation.Collector
	v.OptionalString(validation.TicketSubject, "subject", in.Subject)
	if in.Priority != nil {
		v.Int(validation.TicketPriority, "priority", int64(*in.Priority))
	}
	if in.RequestTypeId != nil {
		v.String(validation.ObjectID, "requestTypeId", *in.RequestTypeId)
	}
	validateOrg(&v, in.OrganisationId)
	var tags []string
	if in.Tags != nil {
		tags = NormalizeTags(&v, "tags", *in.Tags)
	}
	if err := v.Err(); err != nil {
		return Ticket{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	if t.Archived() {
		return Ticket{}, archivedErr(scope, t)
	}
	var cc []string
	if in.CCEmails != nil {
		var cv validation.Collector
		if cc = NormalizeCCEmails(&cv, "ccEmails", *in.CCEmails, t.Requester.Email); cv.Err() != nil {
			return Ticket{}, cv.Err()
		}
	}
	if in.Tags != nil {
		if err := s.checkTagsKnown(ctx, scope.Project.Id, *in.Tags, tags, t.Tags); err != nil {
			return Ticket{}, err
		}
	}
	if integration {
		if t.Source != SourceAPI || t.CreatedBy.Type != audit.ActorService || t.CreatedBy.Id != scope.Principal.Id {
			return Ticket{}, errSubjectLocked()
		}
		expectedVersion = t.Version
	}
	if t.Version != expectedVersion {
		return Ticket{}, errVersion()
	}
	var ch Changes
	var changed, staffChanged []string
	if in.Tags != nil && !slices.Equal(tags, t.Tags) {
		ch.Tags, staffChanged = &tags, append(staffChanged, "tags")
	}
	if in.CCEmails != nil && !slices.Equal(cc, t.CCEmails) {
		ch.CCEmails, staffChanged = &cc, append(staffChanged, "ccEmails")
	}
	if in.Subject != nil && *in.Subject != t.Subject {
		ch.Subject, changed = in.Subject, append(changed, "subject")
	}
	if in.Priority != nil && *in.Priority != t.Priority {
		ch.Priority, changed = in.Priority, append(changed, "priority")
	}
	if in.OrganisationId != nil && *in.OrganisationId != t.organisationID() {
		if t.Source == SourceAPI && scope.Principal.IsUser() {
			return Ticket{}, fault.Forbidden("organisationManagedByIntegration", "The integrating system manages this ticket's organisation.")
		}
		if *in.OrganisationId != "" {
			if err := s.checkOrganisation(ctx, scope, *in.OrganisationId); err != nil {
				return Ticket{}, err
			}
		}
		ch.OrganisationId, changed = in.OrganisationId, append(changed, "organisationId")
	}
	if in.RequestTypeId != nil && *in.RequestTypeId != t.RequestType.Id {
		rt, err := s.requestType(ctx, scope, in.RequestTypeId)
		if err != nil {
			return Ticket{}, err
		}
		ch.RequestTypeId, changed = &rt.Id, append(changed, "requestType")
	}
	if len(changed) == 0 && len(staffChanged) == 0 {
		return s.present(scope, t), nil
	}
	now := s.clock.Now()
	ch.UpdatedAt = now
	m := Mutation{TicketId: t.Id, ExpectedVersion: expectedVersion,
		Audit: []audit.Event{s.event(ctx, scope, t, audit.TicketUpdated, now, append(slices.Clone(changed), staffChanged...)...)}}
	if len(changed) > 0 {
		// Tags are staff working data: they neither move
		// the requester-visible update time nor appear in ticket.updated.
		ch.PublicUpdatedAt = &now
		m.Outbox = []outbox.Event{s.withChanges(s.outboxEvent(scope, outbox.TicketUpdated, t.Id, VisibilityPublic, now), changed)}
	}
	if ch.Priority != nil {
		p := *ch.Priority
		m.SLA = slaFor(func(in *sla.Instance) { in.ChangePriority(p, now) })
	}
	m.Changes = ch
	return s.mutate(ctx, scope, m, nil)
}

// mutate runs a mutation and maps precondition failures. classify, when
// set, inspects the current ticket to explain a failed precondition.
func (s *Service) mutate(ctx context.Context, scope *access.ProjectScope, m Mutation, classify func(Ticket) error) (Ticket, error) {
	m.ProjectId, m.ViewerId = scope.Project.Id, viewerID(scope)
	if m.Uploader.Id == "" {
		m.Uploader = Uploader{Kind: string(scope.Principal.Kind), Id: scope.Principal.Id}
	}
	out, _, err := s.repo.Mutate(ctx, m)
	switch {
	case err == nil:
		return s.present(scope, out), nil
	case errors.Is(err, ErrPrecondition):
		cur, gerr := s.repo.Get(ctx, scope.Project.Id, m.TicketId, viewerID(scope))
		if gerr != nil {
			return Ticket{}, errTicketNotFound()
		}
		if classify != nil {
			if cerr := classify(cur); cerr != nil {
				return Ticket{}, cerr
			}
		}
		if cur.Archived() {
			return Ticket{}, archivedErr(scope, cur)
		}
		return Ticket{}, errVersion()
	case errors.Is(err, ErrIneligible):
		return Ticket{}, fault.Field("assigneeId", "notEligible", "The assignee must be an active member of the ticket's group.")
	case errors.Is(err, ErrNotFound):
		return Ticket{}, errTicketNotFound()
	case errors.Is(err, ErrGroupUnavailable):
		return Ticket{}, errGroupInactive()
	case errors.Is(err, ErrHoldReasonGone):
		return Ticket{}, fault.Field("holdReasonId", "notFound", "No hold reason with this ID exists in the project.")
	case errors.Is(err, ErrOrganisationInactive):
		return Ticket{}, fault.Field("organisationId", "organisationInactive", "This organisation is inactive and cannot be linked to new tickets.")
	case errors.Is(err, ErrOrganisationGone):
		return Ticket{}, fault.Field("organisationId", "notFound", "No organisation with this ID exists in the project.")
	}
	return Ticket{}, err
}

func statusChanges(ch *Changes, p Plan, t Ticket, now time.Time) []string {
	var changed []string
	if p.Status.Id != t.Status.Id {
		cat := p.Status.Category
		ch.StatusId, ch.StatusCategory = &p.Status.Id, &cat
		ch.PublicUpdatedAt = &now // the status is visible to the requester side
		changed = append(changed, "status")
		// Every status change restarts the lifecycle automation timers;
		// only waitingCustomer starts a requester follow-up clock.
		ac := &AutomationChange{}
		if cat == statuses.CategoryWaitingCustomer {
			at := now
			ac.WaitingSince = &at
		}
		ch.Automation = ac
		// Entering On hold records the hold; any other status ends it.
		switch {
		case p.Hold != nil:
			ch.Hold = p.Hold
		case t.StatusCategory == statuses.CategoryOnHold:
			ch.ClearHold = true
		}
	}
	if p.ClearAssignee && t.Assignee != nil {
		empty := ""
		ch.AssigneeId = &empty
		changed = append(changed, "assignee")
	}
	if p.SetResolved {
		at := &now
		ch.ResolvedAt = &at
	}
	if p.ClearResolved {
		var none *time.Time
		ch.ResolvedAt = &none
	}
	if p.SetArchived {
		at := &now
		ch.ArchivedAt = &at
	}
	return changed
}

// Claim atomically assigns an unassigned ticket to the calling agent. The
// ticket stays in its group. Exactly one of several concurrent claims
// succeeds; the others receive 409 ticketAlreadyClaimed.
func (s *Service) Claim(ctx context.Context, scope *access.ProjectScope, ref string, expectedVersion int64) (Ticket, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Ticket{}, err
	}
	if err := scope.Require(access.CapTicketClaim); err != nil {
		return Ticket{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	if t.Archived() {
		return Ticket{}, archivedErr(scope, t)
	}
	if t.Assignee != nil {
		return Ticket{}, errAlreadyClaimed()
	}
	if !scope.InGroup(t.Group.Id) {
		return Ticket{}, fault.Forbidden("notGroupMember", "You can claim tickets only in groups you belong to.")
	}
	if t.Version != expectedVersion {
		return Ticket{}, errVersion()
	}
	set, err := s.statusSet(ctx, scope)
	if err != nil {
		return Ticket{}, err
	}
	plan, err := PlanClaim(t, set)
	if err != nil {
		return Ticket{}, lifecycleErr(scope, t, err)
	}
	now := s.clock.Now()
	me := scope.Principal.Id
	ch := Changes{AssigneeId: &me, UpdatedAt: now}
	changed := append([]string{"assignee"}, statusChanges(&ch, plan, t, now)...)
	return s.mutate(ctx, scope, Mutation{
		TicketId: t.Id, ExpectedVersion: expectedVersion, RequireUnassigned: true,
		RequireCategories: workableCategories(), Eligible: &Eligibility{GroupId: t.Group.Id, UserId: me}, Changes: ch,
		Audit:  []audit.Event{s.event(ctx, scope, t, audit.TicketClaimed, now, changed...)},
		Outbox: []outbox.Event{s.outboxEvent(scope, outbox.TicketClaimed, t.Id, VisibilityAgent, now)},
	}, func(cur Ticket) error {
		if cur.Assignee != nil {
			return errAlreadyClaimed()
		}
		return nil
	})
}

// Release returns an assigned ticket to its group queue. The assignee may
// release their own ticket; admins may release
// anyone's. A reason (ticket.releaseReason) is required; it is recorded on
// the audit event and shown in the activity timeline.
func (s *Service) Release(ctx context.Context, scope *access.ProjectScope, ref string, expectedVersion int64, reason string) (Ticket, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Ticket{}, err
	}
	if err := scope.Require(access.CapTicketClaim); err != nil {
		return Ticket{}, err
	}
	reason = strings.TrimSpace(reason)
	var v validation.Collector
	v.String(validation.TicketReleaseReason, "reason", reason)
	if err := v.Err(); err != nil {
		return Ticket{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	if t.Archived() {
		return Ticket{}, archivedErr(scope, t)
	}
	if t.Assignee == nil {
		return Ticket{}, fault.Conflict("ticketNotAssigned", "The ticket is not assigned to anyone.")
	}
	if t.Assignee.Id != scope.Principal.Id {
		if err := scope.Require(access.CapTicketAssign); err != nil {
			return Ticket{}, err
		}
	}
	if t.Version != expectedVersion {
		return Ticket{}, errVersion()
	}
	set, err := s.statusSet(ctx, scope)
	if err != nil {
		return Ticket{}, err
	}
	plan, err := PlanRelease(t, set)
	if err != nil {
		return Ticket{}, lifecycleErr(scope, t, err)
	}
	now := s.clock.Now()
	ch := Changes{UpdatedAt: now}
	changed := statusChanges(&ch, plan, t, now)
	ev := s.event(ctx, scope, t, audit.TicketReleased, now, changed...)
	ev.Reason = reason
	return s.mutate(ctx, scope, Mutation{
		TicketId: t.Id, ExpectedVersion: expectedVersion, Changes: ch,
		Audit:  []audit.Event{ev},
		Outbox: []outbox.Event{s.outboxEvent(scope, outbox.TicketReleased, t.Id, VisibilityAgent, now)},
	}, nil)
}

// OptionalID distinguishes an absent JSON field from an explicit null.
type OptionalID struct {
	Set   bool
	Value *string
}

// AssignInput is an assignment or group transfer.
type AssignInput struct {
	GroupId string
	// AssigneeID: absent keeps the current assignee when they are eligible
	// in the target group (and clears them otherwise); null clears; a user
	// ID assigns that user, who must be an active member of the group.
	AssigneeId OptionalID
}

// Assign transfers the ticket to a group and/or assigns a person
// (ticket.assign: admins, and agent tokens with tickets:write). The
// assignee must be an active member of the group; an agent token is never
// an assignee (it is not a user).
func (s *Service) Assign(ctx context.Context, scope *access.ProjectScope, ref string, expectedVersion int64, in AssignInput) (Ticket, error) {
	if err := scope.Principal.RequireStaff(); err != nil {
		return Ticket{}, err
	}
	if err := scope.Require(access.CapTicketAssign); err != nil {
		return Ticket{}, err
	}
	var v validation.Collector
	v.String(validation.ObjectID, "groupId", in.GroupId)
	if in.AssigneeId.Value != nil {
		v.String(validation.ObjectID, "assigneeId", *in.AssigneeId.Value)
	}
	if err := v.Err(); err != nil {
		return Ticket{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	if t.Archived() {
		return Ticket{}, archivedErr(scope, t)
	}
	if t.Version != expectedVersion {
		return Ticket{}, errVersion()
	}
	group, active, err := s.repo.ActiveGroup(ctx, scope.Project.Id, in.GroupId)
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, fault.Field("groupId", "notFound", "No group with this ID exists in the project.")
	}
	if err != nil {
		return Ticket{}, err
	}
	if !active {
		return Ticket{}, fault.Field("groupId", "inactive", "The group is inactive.")
	}

	target := ""
	switch {
	case in.AssigneeId.Value != nil:
		target = *in.AssigneeId.Value
	case !in.AssigneeId.Set && t.Assignee != nil:
		ok, err := s.repo.IsEligible(ctx, scope.Project.Id, group.Id, t.Assignee.Id)
		if err != nil {
			return Ticket{}, err
		}
		if ok {
			target = t.Assignee.Id
		}
	}
	if in.AssigneeId.Value != nil {
		ok, err := s.repo.IsEligible(ctx, scope.Project.Id, group.Id, target)
		if err != nil {
			return Ticket{}, err
		}
		if !ok {
			return Ticket{}, fault.Field("assigneeId", "notEligible", "The assignee must be an active member of the target group.")
		}
	}
	if group.Id == t.Group.Id && target == t.AssigneeID() {
		return s.present(scope, t), nil
	}

	set, err := s.statusSet(ctx, scope)
	if err != nil {
		return Ticket{}, err
	}
	plan, err := PlanAssignment(t, set, target)
	if err != nil {
		return Ticket{}, lifecycleErr(scope, t, err)
	}
	now := s.clock.Now()
	ch := Changes{UpdatedAt: now}
	var changed []string
	action := audit.TicketAssigned
	if group.Id != t.Group.Id {
		ch.GroupId, changed, action = &group.Id, append(changed, "group"), audit.TicketTransferred
	}
	changed = append(changed, statusChanges(&ch, plan, t, now)...)
	m := Mutation{TicketId: t.Id, ExpectedVersion: expectedVersion}
	if target != "" {
		if target != t.AssigneeID() {
			ch.AssigneeId = &target
			changed = append(changed, "assignee")
		}
		m.Eligible = &Eligibility{GroupId: group.Id, UserId: target}
	}
	m.Changes = ch
	m.Audit = []audit.Event{s.event(ctx, scope, t, action, now, changed...)}
	m.Outbox = []outbox.Event{s.outboxEvent(scope, outbox.TicketAssigned, t.Id, VisibilityAgent, now)}
	return s.mutate(ctx, scope, m, nil)
}

// TransitionInput is an explicit lifecycle action.
type TransitionInput struct {
	Action   string
	StatusId *string
	Reason   string
	// HoldReasonId is required with the hold action (and refused otherwise);
	// the hold lasts exactly the reason's maxDays. Note is an optional
	// internal note saved with the hold.
	HoldReasonId string
	Note         string
}

// Transition resolves, reopens, archives, puts on hold or sets a workable
// status.
// Archived tickets reject every transition with ticketArchived.
func (s *Service) Transition(ctx context.Context, scope *access.ProjectScope, ref string, expectedVersion int64, in TransitionInput) (Ticket, error) {
	if err := scope.Principal.RequireStaff(); err != nil {
		return Ticket{}, err
	}
	if err := scope.Require(access.CapTicketTransition); err != nil {
		return Ticket{}, err
	}
	var v validation.Collector
	v.String(validation.TicketTransitionAction, "action", in.Action)
	v.String(validation.TicketTransitionReason, "reason", in.Reason)
	switch {
	case in.Action == "setStatus" && in.StatusId == nil:
		v.Add("statusId", validation.CodeRequired, "Choose the status to set.")
	case in.Action != "setStatus" && in.StatusId != nil:
		v.Add("statusId", "notAllowed", "statusId is only accepted with setStatus.")
	case in.StatusId != nil:
		v.String(validation.ObjectID, "statusId", *in.StatusId)
	}
	switch {
	case in.Action == "hold" && in.HoldReasonId == "":
		v.Add("holdReasonId", validation.CodeRequired, "Choose why the ticket is put on hold.")
	case in.Action == "hold":
		v.String(validation.ObjectID, "holdReasonId", in.HoldReasonId)
	case in.HoldReasonId != "":
		v.Add("holdReasonId", "notAllowed", "holdReasonId is only accepted with hold.")
	}
	in.Note = strings.TrimSpace(in.Note)
	switch {
	case in.Note != "" && in.Action != "hold":
		v.Add("note", "notAllowed", "note is only accepted with hold.")
	case in.Note != "":
		s.bodyRule(&v, "note", in.Note)
	}
	if err := v.Err(); err != nil {
		return Ticket{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	if t.Archived() {
		return Ticket{}, archivedErr(scope, t)
	}
	if t.Version != expectedVersion {
		return Ticket{}, errVersion()
	}
	set, err := s.statusSet(ctx, scope)
	if err != nil {
		return Ticket{}, err
	}
	var target *statuses.Status
	if in.StatusId != nil {
		st, ok := set.ByID(*in.StatusId)
		if !ok {
			return Ticket{}, fault.Field("statusId", "notFound", "No active status with this ID exists in the project.")
		}
		target = &st
	}
	eligible := false
	if in.Action == "reopen" && t.Assignee != nil {
		if eligible, err = s.repo.IsEligible(ctx, scope.Project.Id, t.Group.Id, t.Assignee.Id); err != nil {
			return Ticket{}, err
		}
	}
	plan, err := PlanTransition(t, in.Action, target, set, eligible)
	if err != nil {
		return Ticket{}, lifecycleErr(scope, t, err)
	}
	if !plan.Changed(t) {
		return s.present(scope, t), nil
	}
	now := s.clock.Now()
	var holdReason holdreasons.Reason
	if in.Action == "hold" {
		if in.Note != "" {
			if err := scope.Require(access.CapTicketNoteAgent); err != nil {
				return Ticket{}, err
			}
		}
		if holdReason, err = s.repo.HoldReason(ctx, scope.Project.Id, in.HoldReasonId); errors.Is(err, holdreasons.ErrNotFound) {
			return Ticket{}, fault.Field("holdReasonId", "notFound", "No hold reason with this ID exists in the project.")
		} else if err != nil {
			return Ticket{}, err
		}
		plan.Hold = &Hold{ReasonId: holdReason.Id, ReasonName: holdReason.Name, Until: now.Add(time.Duration(holdReason.MaxDays) * 24 * time.Hour)}
	}
	ch := Changes{UpdatedAt: now}
	changed := statusChanges(&ch, plan, t, now)
	var note *Message
	if in.Note != "" {
		ch.StaffActivityAt = &now
		note = &Message{ProjectId: scope.Project.Id, TicketId: t.Id, Visibility: VisibilityAgent, Author: staffAuthor(scope.Principal),
			Body: in.Note, CreatedAt: now}
	}
	m := Mutation{TicketId: t.Id, ExpectedVersion: expectedVersion, Changes: ch, Message: note,
		RequireCategories: categoriesFor(in.Action)}
	if in.Action == "reopen" && eligible && !plan.ClearAssignee {
		m.Eligible = &Eligibility{GroupId: t.Group.Id, UserId: t.Assignee.Id}
	}
	// SLA is first response only: resolving or archiving before a staff
	// reply stops it; reopening and status changes start or change nothing.
	switch in.Action {
	case "resolve":
		m.SLA = slaFor(func(c *sla.Instance) { c.Resolve(now) })
	case "archive":
		m.SLA = slaFor(func(c *sla.Instance) { c.Archive(now) })
	}
	ev := s.event(ctx, scope, t, map[string]audit.Action{"resolve": audit.TicketResolved, "reopen": audit.TicketReopened,
		"archive": audit.TicketArchived, "hold": audit.TicketHeld, "setStatus": audit.TicketStatusChanged}[in.Action], now, changed...)
	ev.Reason = in.Reason
	if in.Action == "hold" {
		ev.Reason = holdReason.Name
		if in.Reason != "" {
			ev.Reason += ": " + in.Reason
		}
	}
	m.Audit = []audit.Event{ev}
	if note != nil {
		mev := audit.New(ctx, s.actor(scope), audit.MessageCreated, "", now)
		mev.ProjectId, mev.TicketId, mev.Changes = scope.Project.Id, t.Id, []string{"visibility:" + VisibilityAgent}
		m.Audit = append(m.Audit, mev)
		m.Outbox = append(m.Outbox, s.outboxEvent(scope, outbox.MessageCreated, t.Id, VisibilityAgent, now))
	}
	sc := s.outboxEvent(scope, outbox.TicketStatusChanged, t.Id, VisibilityPublic, now)
	sc.Transition = in.Action
	m.Outbox = append([]outbox.Event{sc}, m.Outbox...)
	return s.mutate(ctx, scope, m, nil)
}

func categoriesFor(action string) []statuses.Category {
	switch action {
	case "reopen", "archive":
		return []statuses.Category{statuses.CategoryResolved}
	}
	return workableCategories()
}

// ---- Watchers ----

// maxListedWatchers bounds the watcher list (and the per-watcher access
// checks behind it).
const maxListedWatchers = 200

// ListWatchers returns the watchers of a ticket the caller can read who can
// still read it themselves now (active account, project membership and
// group/ticket visibility are re-checked per watcher; watching grants no
// access). Staff only: integrations receive 403 userRequired.
func (s *Service) ListWatchers(ctx context.Context, scope *access.ProjectScope, ref string) ([]Watcher, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return nil, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return nil, err
	}
	all, err := s.repo.TicketWatchers(ctx, t.Id, maxListedWatchers)
	if err != nil {
		return nil, err
	}
	out := make([]Watcher, 0, len(all))
	for _, w := range all {
		if w.Id != scope.Principal.Id {
			ok, err := s.watcherCanRead(ctx, t, w.Id)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, w)
	}
	return out, nil
}

func (s *Service) watcherCanRead(ctx context.Context, t Ticket, userID string) (bool, error) {
	if s.authors == nil {
		return false, nil
	}
	denied := func(err error) bool {
		k := fault.As(err).Kind
		return k == fault.KindNotFound || k == fault.KindForbidden
	}
	us, err := s.authors.UserScope(ctx, t.WorkspaceId, t.ProjectId, userID)
	if err != nil {
		if denied(err) {
			return false, nil
		}
		return false, err
	}
	acc, err := readAccess(us)
	if err != nil {
		if denied(err) {
			return false, nil
		}
		return false, err
	}
	return acc.allows(t), nil
}

// Watch subscribes the signed-in user to a ticket they can already read.
// Watching grants no access. Archived tickets accept no new watchers, but
// unwatching is always allowed.
func (s *Service) Watch(ctx context.Context, scope *access.ProjectScope, ref string, watch bool) (Ticket, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Ticket{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	if watch && t.Archived() {
		return Ticket{}, archivedErr(scope, t)
	}
	if t.Watching == watch {
		return s.present(scope, t), nil
	}
	now := s.clock.Now()
	action := map[bool]audit.Action{true: audit.TicketWatcherAdded, false: audit.TicketWatcherRemoved}[watch]
	ev := s.event(ctx, scope, t, action, now, "watchers")
	out, err := s.repo.SetWatch(ctx, t, scope.Principal.Id, watch, ev)
	if errors.Is(err, ErrTicketArchived) {
		return Ticket{}, archivedErr(scope, t)
	}
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, errTicketNotFound()
	}
	if err != nil {
		return Ticket{}, err
	}
	return s.present(scope, out), nil
}

// OrganisationRef resolves an organisation by ID for system callers such as the webhook
// worker (no permission check; the caller already holds the event).
func (s *Service) OrganisationRef(ctx context.Context, projectID, id string) (OrgRef, error) {
	return s.repo.Organisation(ctx, projectID, id)
}
