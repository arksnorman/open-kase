package tickets

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Link types (stored relation, read from source to target).
const (
	LinkRelated   = "relatedTo"   // symmetric; stored with source < target
	LinkDuplicate = "duplicateOf" // source duplicates target
	LinkFollowUp  = "followUpTo"  // source is a follow-up of target
	LinkCopy      = "copiedFrom"  // source is a copy of target
)

// Outbox event types for link changes (staff-only invalidation).
const (
	OutboxLinkAdded   = outbox.TicketLinkAdded
	OutboxLinkRemoved = outbox.TicketLinkRemoved
	// OutboxMessageRedacted invalidates clients showing the message.
	OutboxMessageRedacted = outbox.MessageRedacted
)

// MaxLinksPerTicket bounds the links of one ticket (either side).
const MaxLinksPerTicket = 100

// Link relates two tickets of the same project. Each side is authorized
// independently: a reader only learns about the other ticket when they may
// read it.
type Link struct {
	Id        string
	ProjectId string
	SourceId  string
	TargetId  string
	Type      string
	CreatedBy audit.Actor
	CreatedAt time.Time
}

// LinkView is a link from one ticket's perspective.
type LinkView struct {
	Link Link
	// Direction is "outgoing" when the viewed ticket is the source.
	Direction string
	// Relation names the link from the viewed ticket: relatedTo,
	// duplicateOf, duplicatedBy, followUpTo, followedUpBy, copiedFrom or
	// copiedTo.
	Relation string
	Other    Ticket
}

func relation(l Link, outgoing bool) string {
	switch {
	case l.Type == LinkDuplicate && !outgoing:
		return "duplicatedBy"
	case l.Type == LinkFollowUp && !outgoing:
		return "followedUpBy"
	case l.Type == LinkCopy && !outgoing:
		return "copiedTo"
	}
	return l.Type
}

func errLinkNotFound() error { return fault.NotFound("linkNotFound") }

func errTooManyLinks() error {
	return fault.Conflict("tooManyLinks", "A ticket can have at most 100 links.")
}

// ListLinks returns the links of an accessible ticket whose other side the
// caller may also read (live tickets only). Links to tickets the caller
// cannot see are omitted entirely.
func (s *Service) ListLinks(ctx context.Context, scope *access.ProjectScope, ref string) ([]LinkView, error) {
	acc, err := readAccess(scope)
	if err != nil {
		return nil, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return nil, err
	}
	links, err := s.repo.TicketLinks(ctx, t.Id, MaxLinksPerTicket*2)
	if err != nil {
		return nil, err
	}
	others := make([]string, 0, len(links))
	for _, l := range links {
		others = append(others, otherSide(l, t.Id))
	}
	rows, err := s.repo.ListByIDs(ctx, scope.Project.Id, others, viewerID(scope))
	if err != nil {
		return nil, err
	}
	byID := map[string]Ticket{}
	for _, o := range rows {
		if acc.allows(o) {
			byID[o.Id] = o
		}
	}
	out := make([]LinkView, 0, len(links))
	for _, l := range links {
		o, ok := byID[otherSide(l, t.Id)]
		if !ok {
			continue
		}
		outgoing := l.SourceId == t.Id
		dir := "incoming"
		if outgoing {
			dir = "outgoing"
		}
		if l.Type == LinkRelated {
			dir = "outgoing" // symmetric
		}
		out = append(out, LinkView{Link: l, Direction: dir, Relation: relation(l, outgoing || l.Type == LinkRelated), Other: s.present(scope, o)})
	}
	return out, nil
}

func otherSide(l Link, id string) string {
	if l.SourceId == id {
		return l.TargetId
	}
	return l.SourceId
}

// LinkInput adds a link from the ticket in the path to another ticket.
type LinkInput struct {
	Type   string
	Ticket string // the other ticket: ID or key
}

// AddLink links two tickets the caller can both read. Links are separate
// records, so archived tickets can be linked (architecture §3.3).
func (s *Service) AddLink(ctx context.Context, scope *access.ProjectScope, ref string, in LinkInput) (LinkView, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return LinkView{}, err
	}
	if err := scope.Require(access.CapTicketTransition); err != nil {
		return LinkView{}, err
	}
	var v validation.Collector
	v.String(validation.TicketLinkType, "type", in.Type)
	if in.Ticket == "" {
		v.Add("ticket", validation.CodeRequired, "Name the ticket to link by key or ID.")
	}
	if err := v.Err(); err != nil {
		return LinkView{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return LinkView{}, err
	}
	other, err := s.load(ctx, scope, in.Ticket)
	if err != nil {
		if fault.As(err).Code == "ticketNotFound" {
			return LinkView{}, fault.Field("ticket", "notFound", "No ticket with this key or ID exists in the project, or you cannot see it.")
		}
		return LinkView{}, err
	}
	if other.Id == t.Id {
		return LinkView{}, fault.Field("ticket", "sameTicket", "A ticket cannot be linked to itself.")
	}
	now := s.clock.Now()
	l := Link{ProjectId: scope.Project.Id, SourceId: t.Id, TargetId: other.Id, Type: in.Type, CreatedBy: s.actor(scope), CreatedAt: now}
	if l.Type == LinkRelated && l.TargetId < l.SourceId {
		l.SourceId, l.TargetId = l.TargetId, l.SourceId
	}
	events, ob := s.linkRecords(ctx, scope, audit.TicketLinkAdded, OutboxLinkAdded, l, []Ticket{t, other}, now)
	out, err := s.repo.InsertLink(ctx, l, events, ob)
	switch {
	case errors.Is(err, ErrLinkExists):
		return LinkView{}, fault.Conflict("linkExists", "These tickets are already linked this way.")
	case errors.Is(err, ErrTooManyLinks):
		return LinkView{}, errTooManyLinks()
	case err != nil:
		return LinkView{}, err
	}
	outgoing := out.SourceId == t.Id || out.Type == LinkRelated
	dir := "incoming"
	if outgoing {
		dir = "outgoing"
	}
	return LinkView{Link: out, Direction: dir, Relation: relation(out, outgoing), Other: s.present(scope, other)}, nil
}

// linkRecords builds one audit event and one outbox event per side.
func (s *Service) linkRecords(ctx context.Context, scope *access.ProjectScope, action audit.Action, typ string, l Link, sides []Ticket, now time.Time) ([]audit.Event, []outbox.Event) {
	var events []audit.Event
	var ob []outbox.Event
	for _, side := range sides {
		ev := audit.New(ctx, s.actor(scope), action, "", now)
		ev.ProjectId, ev.TicketId, ev.Changes = scope.Project.Id, side.Id, []string{"links:" + l.Type}
		events = append(events, ev)
		ob = append(ob, outbox.Event{Type: typ, ProjectId: scope.Project.Id, TicketId: side.Id, Visibility: VisibilityAgent,
			Actor: s.actor(scope), OccurredAt: now})
	}
	return events, ob
}

// RemoveLink deletes a link of the ticket in the path. The caller must be
// able to read both tickets; otherwise the link does not exist for them.
func (s *Service) RemoveLink(ctx context.Context, scope *access.ProjectScope, ref, linkID string) error {
	if err := scope.Principal.RequireUser(); err != nil {
		return err
	}
	if err := scope.Require(access.CapTicketTransition); err != nil {
		return err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return err
	}
	if !validation.ValidID(linkID) {
		return errLinkNotFound()
	}
	l, err := s.repo.GetLink(ctx, scope.Project.Id, linkID)
	if errors.Is(err, ErrNotFound) || (err == nil && l.SourceId != t.Id && l.TargetId != t.Id) {
		return errLinkNotFound()
	}
	if err != nil {
		return err
	}
	other, err := s.load(ctx, scope, otherSide(l, t.Id))
	if err != nil {
		return errLinkNotFound()
	}
	now := s.clock.Now()
	events, ob := s.linkRecords(ctx, scope, audit.TicketLinkRemoved, OutboxLinkRemoved, l, []Ticket{t, other}, now)
	if err := s.repo.DeleteLink(ctx, l, events, ob); errors.Is(err, ErrNotFound) {
		return errLinkNotFound()
	} else if err != nil {
		return err
	}
	return nil
}

// FollowUpInput creates a follow-up ticket. Everything is optional: the
// subject defaults to the original's, the body to a reference to it.
type FollowUpInput struct {
	Subject  *string
	Body     *string
	Priority *int
	// GroupId, RequestTypeId and Tags override the values taken from the
	// original (group, request type) or left empty (tags).
	GroupId       *string
	RequestTypeId *string
	Tags          []string
	// NoSignature skips the author's signatures on the first message.
	NoSignature bool
	// ContactId authors the first message as another contact of the
	// original's organisation. Only when the message is authored as the
	// requester; the new ticket's requester stays the original's.
	ContactId string
}

// FollowUp creates a new ticket linked followUpTo the original (which
// may be archived or in any other state). It copies the subject, requester,
// request type and group (the project's Triage group when that group is no
// longer active) and nothing else: no messages, notes, attachments,
// watchers, tags or assignee. The link is created in the same transaction.
// This is the "Create follow-up ticket" action of ticketArchived.
func (s *Service) FollowUp(ctx context.Context, scope *access.ProjectScope, ref string, in FollowUpInput) (Ticket, error) {
	if err := scope.Require(access.CapTicketCreate); err != nil {
		return Ticket{}, err
	}
	if err := requireOverrides(scope, in.GroupId, in.RequestTypeId, len(in.Tags) > 0); err != nil {
		return Ticket{}, err
	}
	if in.ContactId != "" {
		var v validation.Collector
		v.String(validation.ObjectID, "contactId", in.ContactId)
		if err := v.Err(); err != nil {
			return Ticket{}, err
		}
		if !authorsAsRequester(scope) {
			return Ticket{}, fault.Field("contactId", "notAllowed", "contactId is only valid when the first message is written as the requester.")
		}
	}
	orig, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	contact, hasContact, err := s.organisationContact(ctx, scope, orig, in.ContactId)
	if err != nil {
		return Ticket{}, err
	}
	subject := orig.Subject
	if in.Subject != nil {
		subject = *in.Subject
	}
	body := "Follow-up to " + orig.Ref() + "."
	if in.Body != nil {
		body = *in.Body
	}
	contactID, rtID := orig.Requester.Id, orig.RequestType.Id
	cin := CreateInput{Subject: subject, Body: body, Priority: in.Priority, RequestTypeId: &rtID,
		Requester: RequesterInput{ContactId: &contactID}, OrganisationId: orgIDPtr(orig), keepOrganisation: true, CCEmails: orig.CCEmails}
	cin.Tags = in.Tags
	if in.GroupId != nil {
		cin.GroupId = in.GroupId
	} else if _, active, err := s.repo.ActiveGroup(ctx, scope.Project.Id, orig.Group.Id); err == nil && active {
		gid := orig.Group.Id
		cin.GroupId = &gid
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return Ticket{}, err
	}
	if in.RequestTypeId != nil {
		cin.RequestTypeId = in.RequestTypeId
	} else if _, err := s.requestType(ctx, scope, cin.RequestTypeId); err != nil {
		cin.RequestTypeId = nil // the original's request type was removed
	}
	now := s.clock.Now()
	link := &Link{ProjectId: scope.Project.Id, TargetId: orig.Id, Type: LinkFollowUp, CreatedBy: s.actor(scope), CreatedAt: now}
	ev := audit.New(ctx, s.actor(scope), audit.TicketLinkAdded, "", now)
	ev.ProjectId, ev.TicketId, ev.Changes = scope.Project.Id, orig.Id, []string{"links:" + LinkFollowUp}
	ob := outbox.Event{Type: OutboxLinkAdded, ProjectId: scope.Project.Id, TicketId: orig.Id, Visibility: VisibilityAgent,
		Actor: s.actor(scope), OccurredAt: now}
	return s.create(ctx, scope, cin, &followUp{signed: in.Body != nil && !in.NoSignature, link: link, audit: []audit.Event{ev}, outbox: []outbox.Event{ob}, contact: contactFor(contact, hasContact)})
}

// requireOverrides lets only staff who may edit those fields of a ticket
// (the capability Update needs) set a group, request type or tags while
// creating a follow-up or a copy; requester tokens get 403 staffOnly.
func requireOverrides(scope *access.ProjectScope, group, requestType *string, tags bool) error {
	if group == nil && requestType == nil && !tags {
		return nil
	}
	if !scope.Principal.IsStaff() {
		return fault.Forbidden("staffOnly", "Only support staff can set the group, request type or tags of a new ticket this way.")
	}
	return scope.Require(access.CapTicketTransition)
}

type followUp struct {
	// signed appends the author's signatures to the first public message
	// (a staff user's own text; not the generated default).
	signed bool
	link   *Link
	// copied also audits ticket.copied on the new ticket.
	copied bool
	// contact authors the first message instead of the requester.
	contact *customers.Contact
	audit   []audit.Event
	outbox  []outbox.Event
}

func contactFor(c customers.Contact, ok bool) *customers.Contact {
	if !ok {
		return nil
	}
	return &c
}
