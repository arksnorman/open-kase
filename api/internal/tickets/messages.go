package tickets

import (
	"context"
	"errors"
	"slices"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// maxReplyAttempts bounds internal retries when the ticket changes between
// reading it and committing a message (the reply's status effect is
// recomputed on each attempt).
const maxReplyAttempts = 4

// Message authors of an integration token's post (PostInput.As).
const (
	AsStaff     = "staff"
	AsRequester = "requester"
)

// requireAudience checks that the principal may write to visibility as
// staff: agent notes need ticket.noteAgent (tokens: tickets:internal),
// public staff replies ticket.replyPublic (tokens: tickets:comment).
func requireAudience(scope *access.ProjectScope, visibility string) error {
	if visibility == VisibilityAgent {
		return scope.Require(access.CapTicketNoteAgent)
	}
	return scope.Require(access.CapTicketReplyPublic)
}

// resolveAs decides who authors a post and checks the capability. Users
// always post as themselves. For tokens, as defaults to staff when the
// token holds tickets:comment (or tickets:all) and to requester otherwise;
// "requester" needs tickets:replyAsRequester and a public message.
func resolveAs(scope *access.ProjectScope, in PostInput) (asRequester bool, err error) {
	as := in.As
	if as == "" {
		as = AsStaff
		if scope.Principal.Kind == access.KindIntegration && in.Visibility == VisibilityPublic &&
			!scope.Can(access.CapTicketReplyPublic) {
			as = AsRequester
		}
	}
	if as == AsStaff {
		return false, requireAudience(scope, in.Visibility)
	}
	switch {
	case scope.Principal.Kind != access.KindIntegration:
		return false, fault.Field("as", "notAllowed", "Signed-in users always post as themselves.")
	case in.Visibility != VisibilityPublic:
		return false, fault.Field("as", "notAllowed", "Only public replies can be posted as the requester.")
	case in.ThenStatusId != nil:
		if err := scope.Require(access.CapTicketTransition); err != nil {
			return false, err // a token without tickets:write: 403 as before
		}
		return false, fault.Field("thenStatusId", "notAllowed", "A reply posted as the requester cannot change the status.")
	}
	return true, scope.Require(access.CapTicketReplyAsRequester)
}

// readableAudiences returns the audiences the scope may read.
func readableAudiences(scope *access.ProjectScope) ([]string, error) {
	if err := scope.Require(access.CapMessageReadPublic); err != nil {
		return nil, err
	}
	if scope.Can(access.CapMessageReadPrivate) {
		return []string{VisibilityPublic, VisibilityAgent}, nil
	}
	return []string{VisibilityPublic}, nil
}

// ListMessages pages through the messages the caller may read, oldest
// first. Requester tokens only ever receive public messages.
func (s *Service) ListMessages(ctx context.Context, scope *access.ProjectScope, ref, visibility string, after *pagination.Cursor, limit int) (pagination.Page[Message], error) {
	audiences, err := readableAudiences(scope)
	if err != nil {
		return pagination.Page[Message]{}, err
	}
	if visibility != "" {
		var v validation.Collector
		v.String(validation.MessageVisibility, "visibility", visibility)
		if err := v.Err(); err != nil {
			return pagination.Page[Message]{}, err
		}
		if !slices.Contains(audiences, visibility) {
			return pagination.Page[Message]{Items: []Message{}}, nil
		}
		audiences = []string{visibility}
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return pagination.Page[Message]{}, err
	}
	rows, err := s.repo.ListMessages(ctx, t.Id, audiences, after, limit+1)
	if err != nil {
		return pagination.Page[Message]{}, err
	}
	if !scope.Principal.IsStaff() {
		// Requester tokens see staff authors by name only (as in emails),
		// like the ticket projection that hides the assignee.
		for i := range rows {
			if rows[i].Author.Staff() {
				rows[i].Author.Id = ""
			}
		}
	}
	// The cursor names only the last message; its position (the ticket
	// version it committed with) is resolved server-side, so a reader cannot
	// learn from the cursor how many staff-only changes lie between two
	// public messages.
	return pagination.NewPage(rows, limit, func(m Message) pagination.Cursor {
		return pagination.Cursor{Sort: messageCursorSort, Id: m.Id}
	}), nil
}

// messageCursorSort marks a message cursor that carries no position.
const messageCursorSort = "message"

// PostInput is the post-message command.
type PostInput struct {
	Visibility    string
	Body          string
	AttachmentIds []string
	// ExpectedLastPublicID is the newest public message the author has
	// seen. When another public message arrived since, the post fails with
	// newerReplyExists so the author can review it first.
	ExpectedLastPublicId *string
	// ThenStatusID, when set, changes the ticket's status in the same
	// transaction as the message (staff only, ticket.transition): the
	// built-in Resolved status resolves the ticket, an open/active/waiting
	// status is set like setStatus. An invalid transition fails the whole
	// request and nothing is posted.
	ThenStatusId *string
	// As is who authors an integration token's message: AsStaff (the token
	// as a bot) or AsRequester (the ticket's requester contact); empty
	// chooses staff when the token holds tickets:comment and requester
	// otherwise. Users always post as themselves.
	As string
	// NoSignature skips the author's signatures on a staff user's public
	// reply (REST: addSignature=false). The default appends them.
	NoSignature bool

	// scheduledID is set when a scheduled message is being sent: its
	// reserved attachments are accepted, idempotency replay and draft
	// removal are skipped and the message records the scheduled ID (unique).
	scheduledID string
}

// thenTransition resolves thenStatusId to a lifecycle action.
func thenTransition(set StatusSet, id string) (string, *statuses.Status, error) {
	st, ok := set.ByID(id)
	switch {
	case !ok:
		return "", nil, fault.Field("thenStatusId", "notFound", "No active status with this ID exists in the project.")
	case st.Id == set.Resolved.Id:
		return "resolve", nil, nil
	case st.Category.Workable():
		return "setStatus", &st, nil
	}
	return "", nil, fault.Field("thenStatusId", "notAllowed", "Choose Resolved or an open or awaiting-customer status; hold and archive have their own actions.")
}

// thenErr maps a planning error of thenStatusId.
func thenErr(scope *access.ProjectScope, t Ticket, err error) error {
	if errors.Is(err, ErrStatusNotAllowed) {
		return fault.Field("thenStatusId", "notAllowed", "Choose Resolved or an open or awaiting-customer status; hold and archive have their own actions.")
	}
	return lifecycleErr(scope, t, err)
}

// PostResult is the created message and the ticket after the change.
type PostResult struct {
	Message Message
	Ticket  Ticket
}

// PostMessage appends a public reply or an agent note. The message, the
// ticket's activity, any status effect, attachment linking, audit and
// outbox commit in one transaction.
//
// Agent notes never change the ticket's public activity or status. A
// requester's public reply (posted by an integration) moves a ticket that
// is waiting for the customer, on hold or resolved back to Open (an
// eligible assignee is kept).
func (s *Service) PostMessage(ctx context.Context, scope *access.ProjectScope, ref string, in PostInput) (PostResult, error) {
	in.scheduledID = ""
	return s.postMessage(ctx, scope, ref, in)
}

func (in PostInput) validate(s *Service) error {
	var v validation.Collector
	in.collect(s, &v)
	return v.Err()
}

func (in PostInput) collect(s *Service, v *validation.Collector) {
	v.String(validation.MessageVisibility, "visibility", in.Visibility)
	if in.As != "" {
		v.String(validation.MessageAs, "as", in.As)
	}
	s.bodyRule(v, "body", in.Body)
	validateIDs(v, "attachmentIds", in.AttachmentIds)
	if in.ExpectedLastPublicId != nil && *in.ExpectedLastPublicId != "" {
		v.String(validation.ObjectID, "expectedLastPublicId", *in.ExpectedLastPublicId)
	}
	if in.ThenStatusId != nil {
		v.String(validation.ObjectID, "thenStatusId", *in.ThenStatusId)
	}
}

// requirePost resolves the author (resolveAs) and, with thenStatusId,
// checks the transition capability (tokens: tickets:write).
func requirePost(scope *access.ProjectScope, in PostInput) (asRequester bool, err error) {
	if asRequester, err = resolveAs(scope, in); err != nil {
		return false, err
	}
	if in.ThenStatusId != nil {
		return false, scope.Require(access.CapTicketTransition)
	}
	return asRequester, nil
}

func (s *Service) postMessage(ctx context.Context, scope *access.ProjectScope, ref string, in PostInput) (PostResult, error) {
	if err := in.validate(s); err != nil {
		return PostResult{}, err
	}
	asRequester, err := requirePost(scope, in)
	if err != nil {
		return PostResult{}, err
	}
	p := scope.Principal
	// A retry of a post that already committed returns that message.
	if prior, done, err := s.replayed(ctx, scope, "message/"); in.scheduledID == "" && (done || err != nil) {
		if err != nil {
			return PostResult{}, err
		}
		t, err := s.load(ctx, scope, ref)
		if err != nil {
			return PostResult{}, err
		}
		m, err := s.repo.GetMessage(ctx, t.Id, prior.Id)
		if errors.Is(err, ErrNotFound) {
			return PostResult{}, errTicketNotFound()
		}
		if err != nil {
			return PostResult{}, err
		}
		return PostResult{Message: m, Ticket: s.present(scope, t)}, nil
	}
	uploader := Uploader{Kind: string(p.Kind), Id: p.Id}
	attIDs, err := s.checkAttachmentsFor(ctx, scope, uploader, in.AttachmentIds, in.scheduledID)
	if err != nil {
		return PostResult{}, err
	}
	body := in.Body
	if !asRequester && !in.NoSignature && in.Visibility == VisibilityPublic {
		if body, err = s.signed(ctx, p, body); err != nil {
			return PostResult{}, err
		}
		if err = s.checkSigned(in.Body, body); err != nil {
			return PostResult{}, err
		}
	}
	set, err := s.statusSet(ctx, scope)
	if err != nil {
		return PostResult{}, err
	}
	var thenAction string
	var thenTarget *statuses.Status
	if in.ThenStatusId != nil {
		if thenAction, thenTarget, err = thenTransition(set, *in.ThenStatusId); err != nil {
			return PostResult{}, err
		}
	}

	for attempt := 1; ; attempt++ {
		t, err := s.load(ctx, scope, ref)
		if err != nil {
			return PostResult{}, err
		}
		if t.Archived() {
			return PostResult{}, archivedErr(scope, t)
		}
		if in.ExpectedLastPublicId != nil && *in.ExpectedLastPublicId != t.LastPublicId {
			return PostResult{}, fault.Conflict("newerReplyExists", "A newer public reply was added. Review it before sending; your draft is kept.")
		}
		now := s.clock.Now()
		msg := Message{ProjectId: scope.Project.Id, TicketId: t.Id, Visibility: in.Visibility, Body: body, CreatedAt: now}
		ch := Changes{UpdatedAt: now, StaffActivityAt: &now}
		if in.Visibility == VisibilityPublic {
			ch.PublicActivityAt, ch.PublicUpdatedAt, ch.SetLastPublic = &now, &now, true
		}
		var changed []string
		m := Mutation{TicketId: t.Id, ExpectedVersion: t.Version, AttachmentIds: attIDs, Uploader: uploader, ScheduledId: in.scheduledID}
		reopened := false
		var thenChanged []string
		if !asRequester {
			msg.Author = staffAuthor(p)
			if in.scheduledID == "" && p.IsUser() {
				m.ClearDraftOf = p.Id
			}
			var ops []func(*sla.Instance)
			if in.Visibility == VisibilityPublic {
				// Only a public reply by staff (a user or a token posting as
				// staff) meets the first-response target; agent notes never
				// touch the SLA.
				if t.FirstResponseAt == nil {
					ch.FirstResponseAt = &now
				}
				ops = append(ops, func(c *sla.Instance) { c.StaffPublicReply(now) })
				if t.StatusCategory == statuses.CategoryWaitingCustomer {
					// A new public question restarts the follow-up clock.
					at := now
					ch.Automation = &AutomationChange{WaitingSince: &at}
				}
			}
			if thenAction != "" {
				plan, err := PlanTransition(t, thenAction, thenTarget, set, false)
				if err != nil {
					return PostResult{}, thenErr(scope, t, err)
				}
				if plan.Changed(t) {
					thenChanged = statusChanges(&ch, plan, t, now)
					m.RequireCategories = workableCategories()
					if thenAction == "resolve" {
						ops = append(ops, func(c *sla.Instance) { c.Resolve(now) })
					}
				}
			}
			if len(ops) > 0 {
				m.SLA = slaFor(func(c *sla.Instance) {
					for _, op := range ops {
						op(c)
					}
				})
			}
		} else {
			msg.Author = Author{Kind: AuthorContact, Id: t.Requester.Id, Name: t.Requester.Name, ViaId: p.Id}
			eligible := false
			if t.Assignee != nil {
				if eligible, err = s.repo.IsEligible(ctx, scope.Project.Id, t.Group.Id, t.Assignee.Id); err != nil {
					return PostResult{}, err
				}
			}
			plan, err := PlanRequesterReply(t, set, eligible)
			if err != nil {
				return PostResult{}, lifecycleErr(scope, t, err)
			}
			changed = statusChanges(&ch, plan, t, now)
			if in.Visibility == VisibilityPublic && ch.Automation == nil {
				// A public requester reply cancels pending automation timers
				// (reminders, auto-resolve) in the same transaction.
				ch.Automation = &AutomationChange{}
			}
			if len(changed) > 0 && eligible && !plan.ClearAssignee {
				m.Eligible = &Eligibility{GroupId: t.Group.Id, UserId: t.Assignee.Id}
			}
			// Requester replies never touch the SLA (first response only); a
			// reply that reopens a resolved ticket starts no new cycle.
			reopened = t.StatusCategory == statuses.CategoryResolved && plan.Status.Category != statuses.CategoryResolved
		}
		m.Changes, m.Message = ch, &msg

		ev := audit.New(ctx, s.actor(scope), audit.MessageCreated, "", now)
		ev.ProjectId, ev.TicketId, ev.Changes = scope.Project.Id, t.Id, []string{"visibility:" + in.Visibility}
		if len(attIDs) > 0 {
			ev.Changes = append(ev.Changes, "attachments")
		}
		if in.scheduledID != "" {
			ev.Changes = append(ev.Changes, "scheduled")
		}
		m.Audit = []audit.Event{ev}
		m.Outbox = []outbox.Event{s.outboxEvent(scope, outbox.MessageCreated, t.Id, in.Visibility, now)}
		if len(changed) > 0 {
			action := audit.TicketStatusChanged
			if reopened {
				action = audit.TicketReopened
			}
			sev := s.event(ctx, scope, t, action, now, changed...)
			sev.Reason = "requester reply"
			m.Audit = append(m.Audit, sev)
			sc := s.outboxEvent(scope, outbox.TicketStatusChanged, t.Id, VisibilityPublic, now)
			sc.Transition = outbox.TransitionRequesterReply
			m.Outbox = append(m.Outbox, sc)
		}
		if len(thenChanged) > 0 {
			action := map[string]audit.Action{"resolve": audit.TicketResolved, "setStatus": audit.TicketStatusChanged}[thenAction]
			sev := s.event(ctx, scope, t, action, now, thenChanged...)
			sev.Reason = "set with " + map[string]string{VisibilityPublic: "a public reply", VisibilityAgent: "an agent note"}[in.Visibility]
			m.Audit = append(m.Audit, sev)
			sc := s.outboxEvent(scope, outbox.TicketStatusChanged, t.Id, VisibilityPublic, now)
			sc.Transition = thenAction
			m.Outbox = append(m.Outbox, sc)
		}
		m.ProjectId, m.ViewerId = scope.Project.Id, viewerID(scope)
		if ref, ok := idempotency.RefFrom(ctx); ok && in.scheduledID == "" {
			m.Idempotency = &ref
		}
		out, created, err := s.repo.Mutate(ctx, m)
		switch {
		case err == nil:
			return PostResult{Message: created, Ticket: s.present(scope, out)}, nil
		case errors.Is(err, ErrAttachmentsGone):
			return PostResult{}, errAttachmentsGone()
		case (errors.Is(err, ErrPrecondition) || errors.Is(err, ErrIneligible)) && attempt < maxReplyAttempts:
			continue // the ticket changed: recompute against the new state
		case errors.Is(err, ErrPrecondition) || errors.Is(err, ErrIneligible):
			return PostResult{}, errVersion()
		case errors.Is(err, ErrNotFound):
			return PostResult{}, errTicketNotFound()
		}
		return PostResult{}, err
	}
}

// ---- Drafts ----

func (s *Service) draftTicket(ctx context.Context, scope *access.ProjectScope, ref, visibility string) (Ticket, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Ticket{}, err
	}
	var v validation.Collector
	v.String(validation.MessageVisibility, "visibility", visibility)
	if err := v.Err(); err != nil {
		return Ticket{}, err
	}
	if err := requireAudience(scope, visibility); err != nil {
		return Ticket{}, err
	}
	return s.load(ctx, scope, ref)
}

// GetDraft returns the caller's draft for one audience, or an empty draft
// (version 0) when none is stored.
func (s *Service) GetDraft(ctx context.Context, scope *access.ProjectScope, ref, visibility string) (Draft, error) {
	t, err := s.draftTicket(ctx, scope, ref, visibility)
	if err != nil {
		return Draft{}, err
	}
	d, err := s.repo.GetDraft(ctx, t.Id, scope.Principal.Id, visibility)
	if errors.Is(err, ErrNotFound) {
		return Draft{TicketId: t.Id, AuthorId: scope.Principal.Id, Visibility: visibility}, nil
	}
	return d, err
}

// PutDraft stores the caller's draft. expectedVersion 0 skips the check;
// otherwise a stale version fails with versionConflict (another tab saved).
func (s *Service) PutDraft(ctx context.Context, scope *access.ProjectScope, ref, visibility, body string, baseLastPublicID *string, expectedVersion int64) (Draft, error) {
	t, err := s.draftTicket(ctx, scope, ref, visibility)
	if err != nil {
		return Draft{}, err
	}
	var v validation.Collector
	v.String(validation.DraftBody, "body", body)
	if baseLastPublicID != nil && *baseLastPublicID != "" {
		v.String(validation.ObjectID, "baseLastPublicId", *baseLastPublicID)
	}
	if err := v.Err(); err != nil {
		return Draft{}, err
	}
	if t.Archived() {
		return Draft{}, archivedErr(scope, t)
	}
	d := Draft{TicketId: t.Id, AuthorId: scope.Principal.Id, Visibility: visibility, Body: body,
		BaseLastPublicId: deref(baseLastPublicID), UpdatedAt: s.clock.Now()}
	out, err := s.repo.PutDraft(ctx, d, expectedVersion)
	if errors.Is(err, ErrDraftConflict) {
		return Draft{}, fault.Conflict("versionConflict", "The draft was saved elsewhere. Reload it before saving again.")
	}
	return out, err
}

// DeleteDraft discards the caller's draft (idempotent).
func (s *Service) DeleteDraft(ctx context.Context, scope *access.ProjectScope, ref, visibility string) error {
	t, err := s.draftTicket(ctx, scope, ref, visibility)
	if err != nil {
		return err
	}
	return s.repo.DeleteDraft(ctx, t.Id, scope.Principal.Id, visibility)
}
