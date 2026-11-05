package tickets

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Scheduled message states. Only scheduled and needsReview are active;
// sent and cancelled are final.
const (
	ScheduledPending     = "scheduled"
	ScheduledNeedsReview = "needsReview"
	ScheduledSent        = "sent"
	ScheduledCancelled   = "cancelled"
)

// Reasons recorded with needsReview and cancelled states.
const (
	ReasonNewerRequesterReply = "newerRequesterReply"
	ReasonStatusNotAllowed    = "statusChangeNotAllowed"
	ReasonInvalidMessage      = "invalidMessage"
	ReasonTicketArchived      = "ticketArchived"
	ReasonTicketMissing       = "ticketMissing" // the ticket no longer exists
	ReasonAuthorNoAccess      = "authorNoAccess"
	ReasonAttachmentsGone     = "attachmentsUnavailable"
	ReasonCancelledByAuthor   = "cancelledByAuthor"
	ReasonCancelledByAdmin    = "cancelledByAdmin"
)

// KindScheduledSend is the job that sends a scheduled message at sendAt.
const KindScheduledSend = "scheduledMessage.send"

// Outbox events of scheduled messages: staff-only (agent visibility), no
// webhooks. Both email the author (link only), except a cancellation the
// author made themselves.
const (
	OutboxScheduledNeedsReview = outbox.ScheduledMessageNeedsReview
	OutboxScheduledCancelled   = outbox.ScheduledMessageCancelled
)

// Scheduling limits.
const (
	// MaxScheduleAhead bounds sendAt.
	MaxScheduleAhead = 365 * 24 * time.Hour
	// MaxActiveScheduled bounds one author's active scheduled messages on
	// one ticket (reserved attachments do not expire meanwhile).
	MaxActiveScheduled = 10
)

// ErrScheduledSent means a message for the scheduled message already exists
// (unique index); the delivery is not repeated.
var ErrScheduledSent = errors.New("tickets: scheduled message already sent")

// ScheduledMessage is a reply or agent note that is sent later by the
// durable job KindScheduledSend, through the same command as an immediate
// message. It is private to its author (administrators may list
// and cancel it) and is never visible to integrations; it changes nothing
// on the ticket (counts, SLA, activity) until it is actually sent.
type ScheduledMessage struct {
	Id            string
	WorkspaceId   string
	ProjectId     string
	TicketId      string
	AuthorId      string
	AuthorName    string // resolved at read time
	Visibility    string
	Body          string
	AttachmentIds []string
	Attachments   []Attachment // resolved at read time
	ThenStatusId  string
	// NoSignature skips the author's signature when the message is sent.
	NoSignature bool
	SendAt      time.Time
	State       string
	Reason      string
	// BaseLastPublicID is the newest public message when the author
	// scheduled (or last reviewed) it; a newer requester reply stops the
	// automatic send (needsReview).
	BaseLastPublicId string
	TicketVersion    int64
	// Revision changes with every reschedule, edit, cancel and send; a job
	// for an older revision does nothing.
	Revision      int64
	SentMessageId string
	SentAt        *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Version       int64
}

// Active reports whether the scheduled message can still be sent.
func (m ScheduledMessage) Active() bool {
	return m.State == ScheduledPending || m.State == ScheduledNeedsReview
}

// ScheduledFilter selects a scheduled message for a conditional update.
type ScheduledFilter struct {
	ProjectId string
	Id        string
	Version   int64    // 0 = any
	Revision  int64    // 0 = any
	States    []string // empty = any
}

// ScheduledChanges are the fields an update sets. Nil pointers are kept.
type ScheduledChanges struct {
	Body             *string
	ThenStatusId     *string // "" clears
	NoSignature      *bool
	SendAt           *time.Time
	State            *string
	Reason           *string
	BaseLastPublicId *string
	SentMessageId    *string
	SentAt           *time.Time
	BumpRevision     bool
	// ReleaseUntil returns reserved attachments to the uploader's staging
	// area, expiring at this time.
	ReleaseUntil *time.Time
	UpdatedAt    time.Time
}

// ScheduledRepository persists scheduled messages (implemented by the
// tickets repository).
type ScheduledRepository interface {
	// Transaction runs fn in one transaction (joining the caller's).
	Transaction(ctx context.Context, fn func(ctx context.Context) error) error
	// InsertScheduled stores the record, reserves its staged attachments
	// (ErrAttachmentsGone unless every one is still a staged, unexpired,
	// unreserved upload of the author) and removes the author's draft for
	// the audience.
	InsertScheduled(ctx context.Context, m ScheduledMessage) (ScheduledMessage, error)
	GetScheduled(ctx context.Context, projectID, id string) (ScheduledMessage, error)
	// ListScheduled lists a ticket's scheduled messages by send time;
	// authorID "" lists every author's.
	ListScheduled(ctx context.Context, ticketID, authorID string, states []string, limit int) ([]ScheduledMessage, error)
	CountActiveScheduled(ctx context.Context, ticketID, authorID string) (int64, error)
	// UpdateScheduled applies ch when f matches, else ErrPrecondition.
	UpdateScheduled(ctx context.Context, f ScheduledFilter, ch ScheduledChanges) (ScheduledMessage, error)
	// NewerRequesterReply reports whether a public message by the requester
	// was posted after afterMessageID ("" = any).
	NewerRequesterReply(ctx context.Context, ticketID, afterMessageID string) (bool, error)
	// TicketEvents records audit and outbox events with a snapshot of the
	// ticket, without changing it.
	TicketEvents(ctx context.Context, projectID, ticketID string, events []audit.Event, ob []outbox.Event) error
}

// AuthorScopes re-authorizes a scheduled message's author when the job
// sends it: the user must still be active and a member of the project.
type AuthorScopes interface {
	// UserScope returns the user's current project scope; an error with
	// fault kind NotFound/Forbidden means no access.
	UserScope(ctx context.Context, workspaceID, projectID, userID string) (*access.ProjectScope, error)
}

func errScheduledNotFound() error { return fault.NotFound("scheduledMessageNotFound") }

func errScheduledVersion() error {
	return fault.Conflict("versionConflict", "The scheduled message was changed elsewhere (or it was sent or cancelled). Reload it.")
}

func errScheduledNotActive(m ScheduledMessage) error {
	return fault.Conflict("scheduledMessageNotActive", "This scheduled message was already "+m.State+".")
}

func (s *Service) requireScheduling() error {
	if s.jobs == nil {
		return fault.New(fault.KindUnavailable, "schedulingUnavailable", "Scheduled sending is not available.")
	}
	return nil
}

// ScheduleInput schedules a message.
type ScheduleInput struct {
	PostInput
	SendAt time.Time
}

func (s *Service) checkSendAt(v *validation.Collector, at time.Time) {
	now := s.clock.Now()
	switch {
	case at.IsZero():
		v.Add("sendAt", validation.CodeRequired, "Choose when to send.")
	case !at.After(now):
		v.Add("sendAt", "inPast", "Choose a time in the future.")
	case at.After(now.Add(MaxScheduleAhead)):
		v.Add("sendAt", validation.CodeOutOfRange, "Schedule at most 365 days ahead.")
	}
}

// validateThen checks thenStatusId against the catalog (the transition
// itself is checked when the message is sent).
func (s *Service) validateThen(ctx context.Context, scope *access.ProjectScope, id string) error {
	set, err := s.statusSet(ctx, scope)
	if err != nil {
		return err
	}
	_, _, err = thenTransition(set, id)
	return err
}

func sendJob(m ScheduledMessage) jobs.NewJob {
	rev := strconv.FormatInt(m.Revision, 10)
	return jobs.NewJob{WorkspaceId: m.WorkspaceId, ProjectId: m.ProjectId, Kind: KindScheduledSend,
		Key:     "scheduled-send:" + m.Id + ":" + rev,
		Payload: map[string]string{"projectId": m.ProjectId, "scheduledId": m.Id, "revision": rev},
		RunAt:   m.SendAt}
}

func (s *Service) scheduledEvent(ctx context.Context, actor audit.Actor, m ScheduledMessage, action audit.Action, now time.Time, changes ...string) audit.Event {
	ev := audit.New(ctx, actor, action, "scheduledMessage/"+m.Id, now)
	ev.ProjectId, ev.TicketId, ev.Changes = m.ProjectId, m.TicketId, changes
	return ev
}

func (s *Service) wake(ctx context.Context) {
	if s.wakeJobs != nil {
		s.wakeJobs(ctx)
	}
}

// Schedule stores a message to be sent at in.SendAt by the author. The
// audience, thenStatusId and attachments are checked like an immediate
// post; staged attachments are reserved (they no longer expire) and the
// author's draft for the audience is removed.
func (s *Service) Schedule(ctx context.Context, scope *access.ProjectScope, ref string, in ScheduleInput) (ScheduledMessage, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return ScheduledMessage{}, err
	}
	if err := s.requireScheduling(); err != nil {
		return ScheduledMessage{}, err
	}
	var v validation.Collector
	in.PostInput.collect(s, &v)
	s.checkSendAt(&v, in.SendAt)
	if err := v.Err(); err != nil {
		return ScheduledMessage{}, err
	}
	if _, err := requirePost(scope, in.PostInput); err != nil {
		return ScheduledMessage{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return ScheduledMessage{}, err
	}
	if t.Archived() {
		return ScheduledMessage{}, archivedErr(scope, t)
	}
	if in.ExpectedLastPublicId != nil && *in.ExpectedLastPublicId != t.LastPublicId {
		return ScheduledMessage{}, fault.Conflict("newerReplyExists", "A newer public reply was added. Review it before scheduling; your draft is kept.")
	}
	then := ""
	if in.ThenStatusId != nil {
		if err := s.validateThen(ctx, scope, *in.ThenStatusId); err != nil {
			return ScheduledMessage{}, err
		}
		then = *in.ThenStatusId
	}
	p := scope.Principal
	attIDs, err := s.checkAttachments(ctx, scope, Uploader{Kind: string(p.Kind), Id: p.Id}, in.AttachmentIds)
	if err != nil {
		return ScheduledMessage{}, err
	}
	n, err := s.repo.CountActiveScheduled(ctx, t.Id, p.Id)
	if err != nil {
		return ScheduledMessage{}, err
	}
	if n >= MaxActiveScheduled {
		return ScheduledMessage{}, fault.Conflict("tooManyScheduled",
			"You already have "+strconv.Itoa(MaxActiveScheduled)+" scheduled messages on this ticket. Send or cancel one first.")
	}
	now := s.clock.Now()
	m := ScheduledMessage{WorkspaceId: p.WorkspaceId, ProjectId: scope.Project.Id, TicketId: t.Id, AuthorId: p.Id, AuthorName: p.Name,
		Visibility: in.Visibility, Body: in.Body, AttachmentIds: attIDs, ThenStatusId: then, NoSignature: in.NoSignature, SendAt: in.SendAt.UTC(),
		State: ScheduledPending, BaseLastPublicId: t.LastPublicId, TicketVersion: t.Version, Revision: 1,
		CreatedAt: now, UpdatedAt: now, Version: 1}
	var out ScheduledMessage
	err = s.repo.Transaction(ctx, func(ctx context.Context) error {
		var err error
		if out, err = s.repo.InsertScheduled(ctx, m); err != nil {
			return err
		}
		changes := []string{"visibility:" + m.Visibility, "sendAt"}
		if then != "" {
			changes = append(changes, "thenStatus")
		}
		if len(attIDs) > 0 {
			changes = append(changes, "attachments")
		}
		if err := s.repo.TicketEvents(ctx, m.ProjectId, m.TicketId,
			[]audit.Event{s.scheduledEvent(ctx, s.actor(scope), out, audit.ScheduledMessageCreated, now, changes...)}, nil); err != nil {
			return err
		}
		_, err = s.jobs.Enqueue(ctx, sendJob(out))
		return err
	})
	if errors.Is(err, ErrAttachmentsGone) {
		return ScheduledMessage{}, errAttachmentsGone()
	}
	if err != nil {
		return ScheduledMessage{}, err
	}
	s.wake(ctx)
	return s.hydrateScheduled(ctx, scope, out)
}

func (s *Service) hydrateScheduled(ctx context.Context, scope *access.ProjectScope, m ScheduledMessage) (ScheduledMessage, error) {
	if len(m.AttachmentIds) == 0 {
		return m, nil
	}
	atts, err := s.repo.StagedAttachments(ctx, scope.Project.Id, m.AttachmentIds)
	if err != nil {
		return ScheduledMessage{}, err
	}
	m.Attachments = atts
	return m, nil
}

// ListScheduled returns the caller's scheduled messages on a ticket
// (administrators: everyone's). all=false lists only active ones
// (scheduled, needsReview); all=true includes sent and cancelled (newest
// 100).
func (s *Service) ListScheduled(ctx context.Context, scope *access.ProjectScope, ref string, all bool) ([]ScheduledMessage, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return nil, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return nil, err
	}
	author := scope.Principal.Id
	if scope.Can(access.CapProjectManage) {
		author = ""
	}
	states := []string{ScheduledPending, ScheduledNeedsReview}
	if all {
		states = nil
	}
	rows, err := s.repo.ListScheduled(ctx, t.Id, author, states, 100)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i], err = s.hydrateScheduled(ctx, scope, rows[i]); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// loadScheduled returns a scheduled message of an accessible ticket that
// the caller may see: the author, or (admin=true) a administrator.
func (s *Service) loadScheduled(ctx context.Context, scope *access.ProjectScope, ref, id string, admin bool) (Ticket, ScheduledMessage, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Ticket{}, ScheduledMessage{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, ScheduledMessage{}, err
	}
	if !validation.ValidID(id) {
		return Ticket{}, ScheduledMessage{}, errScheduledNotFound()
	}
	m, err := s.repo.GetScheduled(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) || (err == nil && m.TicketId != t.Id) {
		return Ticket{}, ScheduledMessage{}, errScheduledNotFound()
	}
	if err != nil {
		return Ticket{}, ScheduledMessage{}, err
	}
	if m.AuthorId != scope.Principal.Id && !(admin && scope.Can(access.CapProjectManage)) {
		return Ticket{}, ScheduledMessage{}, errScheduledNotFound()
	}
	return t, m, nil
}

// ScheduleUpdate edits an active scheduled message (author only). Any
// update acknowledges the conversation as reviewed: the state returns to
// scheduled and newer requester replies are measured from now on.
type ScheduleUpdate struct {
	Body   *string
	SendAt *time.Time
	// AddSignature changes whether the signatures are added when sent (nil: keep).
	AddSignature *bool
	// ThenStatusID: Set with nil Value clears it.
	ThenStatusId OptionalID
}

// UpdateScheduled applies an author's edit at expectedVersion.
func (s *Service) UpdateScheduled(ctx context.Context, scope *access.ProjectScope, ref, id string, expectedVersion int64, in ScheduleUpdate) (ScheduledMessage, error) {
	if err := s.requireScheduling(); err != nil {
		return ScheduledMessage{}, err
	}
	t, m, err := s.loadScheduled(ctx, scope, ref, id, false)
	if err != nil {
		return ScheduledMessage{}, err
	}
	var v validation.Collector
	if in.Body != nil {
		s.bodyRule(&v, "body", *in.Body)
	}
	sendAt := m.SendAt
	if in.SendAt != nil {
		sendAt = in.SendAt.UTC()
	}
	s.checkSendAt(&v, sendAt)
	if in.ThenStatusId.Value != nil {
		v.String(validation.ObjectID, "thenStatusId", *in.ThenStatusId.Value)
	}
	if err := v.Err(); err != nil {
		return ScheduledMessage{}, err
	}
	if in.ThenStatusId.Value != nil {
		if err := scope.Require(access.CapTicketTransition); err != nil {
			return ScheduledMessage{}, err
		}
		if err := s.validateThen(ctx, scope, *in.ThenStatusId.Value); err != nil {
			return ScheduledMessage{}, err
		}
	}
	if err := requireAudience(scope, m.Visibility); err != nil {
		return ScheduledMessage{}, err
	}
	if t.Archived() {
		return ScheduledMessage{}, archivedErr(scope, t)
	}
	if m.Version != expectedVersion {
		return ScheduledMessage{}, errScheduledVersion()
	}
	if !m.Active() {
		return ScheduledMessage{}, errScheduledNotActive(m)
	}
	now := s.clock.Now()
	pending, empty := ScheduledPending, ""
	ch := ScheduledChanges{Body: in.Body, SendAt: &sendAt, State: &pending, Reason: &empty, BaseLastPublicId: &t.LastPublicId,
		BumpRevision: true, UpdatedAt: now}
	changes := []string{}
	if in.Body != nil {
		changes = append(changes, "body")
	}
	if in.SendAt != nil {
		changes = append(changes, "sendAt")
	}
	if in.AddSignature != nil {
		no := !*in.AddSignature
		ch.NoSignature = &no
		changes = append(changes, "signature")
	}
	if in.ThenStatusId.Set {
		val := ""
		if in.ThenStatusId.Value != nil {
			val = *in.ThenStatusId.Value
		}
		ch.ThenStatusId = &val
		changes = append(changes, "thenStatus")
	}
	if m.State == ScheduledNeedsReview {
		changes = append(changes, "reviewed")
	}
	var out ScheduledMessage
	err = s.repo.Transaction(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.repo.UpdateScheduled(ctx, ScheduledFilter{ProjectId: m.ProjectId, Id: m.Id, Version: expectedVersion,
			States: []string{ScheduledPending, ScheduledNeedsReview}}, ch)
		if err != nil {
			return err
		}
		if err := s.repo.TicketEvents(ctx, m.ProjectId, m.TicketId,
			[]audit.Event{s.scheduledEvent(ctx, s.actor(scope), out, audit.ScheduledMessageUpdated, now, changes...)}, nil); err != nil {
			return err
		}
		_, err = s.jobs.Enqueue(ctx, sendJob(out))
		return err
	})
	if errors.Is(err, ErrPrecondition) {
		return ScheduledMessage{}, errScheduledVersion()
	}
	if err != nil {
		return ScheduledMessage{}, err
	}
	s.wake(ctx)
	return s.hydrateScheduled(ctx, scope, out)
}

// CancelScheduled cancels an active scheduled message (author, or a
// administrator). Its reserved attachments return to the author's
// staging area and expire normally. expectedVersion 0 skips the check.
func (s *Service) CancelScheduled(ctx context.Context, scope *access.ProjectScope, ref, id string, expectedVersion int64) (ScheduledMessage, error) {
	_, m, err := s.loadScheduled(ctx, scope, ref, id, true)
	if err != nil {
		return ScheduledMessage{}, err
	}
	if expectedVersion > 0 && m.Version != expectedVersion {
		return ScheduledMessage{}, errScheduledVersion()
	}
	if !m.Active() {
		return ScheduledMessage{}, errScheduledNotActive(m)
	}
	reason := ReasonCancelledByAuthor
	if m.AuthorId != scope.Principal.Id {
		reason = ReasonCancelledByAdmin
	}
	out, err := s.finishScheduled(ctx, s.actor(scope), m, ScheduledFilter{ProjectId: m.ProjectId, Id: m.Id, Version: expectedVersion,
		States: []string{ScheduledPending, ScheduledNeedsReview}}, ScheduledCancelled, reason)
	if errors.Is(err, ErrPrecondition) {
		return ScheduledMessage{}, errScheduledVersion()
	}
	if err != nil {
		return ScheduledMessage{}, err
	}
	return s.hydrateScheduled(ctx, scope, out)
}

// finishScheduled moves a scheduled message to cancelled or needsReview
// with a reason (one transaction with its audit event and outbox event).
// Cancelling releases the reserved attachments; needsReview notifies the
// author by email (outbox) unless the author made the change.
func (s *Service) finishScheduled(ctx context.Context, actor audit.Actor, m ScheduledMessage, f ScheduledFilter, state, reason string) (ScheduledMessage, error) {
	now := s.clock.Now()
	ch := ScheduledChanges{State: &state, Reason: &reason, BumpRevision: true, UpdatedAt: now}
	action, typ := audit.ScheduledMessageNeedsReview, OutboxScheduledNeedsReview
	if state == ScheduledCancelled {
		until := now.Add(StagingTTL)
		ch.ReleaseUntil = &until
		action, typ = audit.ScheduledMessageCancelled, OutboxScheduledCancelled
	}
	var out ScheduledMessage
	err := s.repo.Transaction(ctx, func(ctx context.Context) error {
		var err error
		if out, err = s.repo.UpdateScheduled(ctx, f, ch); err != nil {
			return err
		}
		ev := s.scheduledEvent(ctx, actor, out, action, now, "state:"+state)
		ev.Reason = reason
		ob := outbox.Event{Type: typ, ProjectId: m.ProjectId, TicketId: m.TicketId, Visibility: VisibilityAgent, Actor: actor,
			OccurredAt: now, UserId: m.AuthorId, Changes: []string{reason, "visibility:" + m.Visibility}}
		return s.repo.TicketEvents(ctx, m.ProjectId, m.TicketId, []audit.Event{ev}, []outbox.Event{ob})
	})
	return out, err
}

// SendScheduledNow sends an active scheduled message immediately (author
// only), also from needsReview: the author has reviewed the conversation.
// expectedLastPublicID, when sent, guards against replies newer than the
// author's view (409 newerReplyExists). expectedVersion 0 skips the check.
func (s *Service) SendScheduledNow(ctx context.Context, scope *access.ProjectScope, ref, id string, expectedVersion int64, expectedLastPublicID *string) (PostResult, error) {
	_, m, err := s.loadScheduled(ctx, scope, ref, id, false)
	if err != nil {
		return PostResult{}, err
	}
	if expectedVersion > 0 && m.Version != expectedVersion {
		return PostResult{}, errScheduledVersion()
	}
	if !m.Active() {
		return PostResult{}, errScheduledNotActive(m)
	}
	in := m.postInput()
	in.ExpectedLastPublicId = expectedLastPublicID
	var res PostResult
	err = s.repo.Transaction(ctx, func(ctx context.Context) error {
		var err error
		res, err = s.deliverScheduled(ctx, scope, m, ScheduledFilter{ProjectId: m.ProjectId, Id: m.Id, Version: expectedVersion,
			Revision: m.Revision, States: []string{ScheduledPending, ScheduledNeedsReview}}, in)
		return err
	})
	switch {
	case errors.Is(err, ErrPrecondition), errors.Is(err, ErrScheduledSent):
		return PostResult{}, errScheduledVersion()
	case err != nil:
		return PostResult{}, err
	}
	return res, nil
}

func (m ScheduledMessage) postInput() PostInput {
	in := PostInput{Visibility: m.Visibility, Body: m.Body, AttachmentIds: m.AttachmentIds, scheduledID: m.Id, NoSignature: m.NoSignature}
	if m.ThenStatusId != "" {
		then := m.ThenStatusId
		in.ThenStatusId = &then
	}
	return in
}

// deliverScheduled posts the message and marks the record sent, inside the
// caller's transaction (both commit or neither).
func (s *Service) deliverScheduled(ctx context.Context, scope *access.ProjectScope, m ScheduledMessage, f ScheduledFilter, in PostInput) (PostResult, error) {
	res, err := s.postMessage(ctx, scope, m.TicketId, in)
	if err != nil {
		return PostResult{}, err
	}
	sent := ScheduledSent
	at := res.Message.CreatedAt
	_, err = s.repo.UpdateScheduled(ctx, f, ScheduledChanges{State: &sent, SentMessageId: &res.Message.Id, SentAt: &at,
		BumpRevision: true, UpdatedAt: at})
	return res, err
}

// ---- the send job ----

// HandleScheduledSend is the jobs.Handler for KindScheduledSend. Everything
// happens in the job's fenced completion transaction: the record is read
// again, the author re-authorized and the ticket re-checked; then the
// message is posted (same command as an immediate post) and the record
// marked sent, or the record is cancelled / held for review with a reason.
// A stale revision or a final state makes the job a no-op; a worker whose
// lease was taken over commits nothing; the unique scheduledMessageId on
// messages makes a second delivery impossible.
func (s *Service) HandleScheduledSend(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	projectID, id := j.Payload["projectId"], j.Payload["scheduledId"]
	rev, err := strconv.ParseInt(j.Payload["revision"], 10, 64)
	if !validation.ValidID(projectID) || !validation.ValidID(id) || err != nil {
		return jobs.Result{Outcome: jobs.Dead, ErrorClass: "invalidPayload", Error: "the scheduled message payload is invalid"}
	}
	return jobs.Result{Outcome: jobs.Done, Commit: func(ctx context.Context, f jobs.Finish) error {
		if f.State != jobs.StateSucceeded {
			return nil
		}
		err := s.runScheduled(ctx, projectID, id, rev)
		if errors.Is(err, ErrScheduledSent) {
			return nil
		}
		return err
	}}
}

func (s *Service) runScheduled(ctx context.Context, projectID, id string, rev int64) error {
	m, err := s.repo.GetScheduled(ctx, projectID, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Revision != rev || m.State != ScheduledPending {
		return nil // edited, cancelled, sent or held meanwhile
	}
	if s.clock.Now().Before(m.SendAt) {
		return nil // a newer revision's job owns the new time
	}
	filter := ScheduledFilter{ProjectId: projectID, Id: id, Revision: rev, States: []string{ScheduledPending}}
	scope, state, reason, err := s.scheduledCheck(ctx, m)
	if err != nil {
		return err
	}
	if state != "" {
		_, err := s.finishScheduled(ctx, audit.System(audit.SystemScheduler, "scheduled send"), m, filter, state, reason)
		if errors.Is(err, ErrPrecondition) {
			return nil
		}
		return err
	}
	_, err = s.deliverScheduled(ctx, scope, m, filter, m.postInput())
	return err
}

// scheduledCheck decides, at send time, whether the author may still send
// the message: it returns the author's scope, or the state and reason to
// record instead (cancelled or needsReview).
func (s *Service) scheduledCheck(ctx context.Context, m ScheduledMessage) (*access.ProjectScope, string, string, error) {
	cancel := func(reason string) (*access.ProjectScope, string, string, error) {
		return nil, ScheduledCancelled, reason, nil
	}
	review := func(reason string) (*access.ProjectScope, string, string, error) {
		return nil, ScheduledNeedsReview, reason, nil
	}
	// The ticket itself first: missing or archived tickets get nothing.
	t, err := s.repo.Get(ctx, m.ProjectId, m.TicketId, "")
	if errors.Is(err, ErrNotFound) {
		return cancel(ReasonTicketMissing)
	}
	if err != nil {
		return nil, "", "", err
	}
	if t.Archived() {
		return cancel(ReasonTicketArchived)
	}
	if s.authors == nil {
		return cancel(ReasonAuthorNoAccess)
	}
	scope, err := s.authors.UserScope(ctx, m.WorkspaceId, m.ProjectId, m.AuthorId)
	if err != nil {
		if k := fault.As(err).Kind; k == fault.KindNotFound || k == fault.KindForbidden {
			return cancel(ReasonAuthorNoAccess)
		}
		return nil, "", "", err
	}
	in := m.postInput()
	if _, err := requirePost(scope, in); err != nil {
		return cancel(ReasonAuthorNoAccess)
	}
	if _, err := s.load(ctx, scope, m.TicketId); err != nil {
		if k := fault.As(err).Kind; k == fault.KindNotFound || k == fault.KindForbidden {
			return cancel(ReasonAuthorNoAccess)
		}
		return nil, "", "", err
	}
	newer, err := s.repo.NewerRequesterReply(ctx, m.TicketId, m.BaseLastPublicId)
	if err != nil {
		return nil, "", "", err
	}
	if newer {
		return review(ReasonNewerRequesterReply)
	}
	if err := in.validate(s); err != nil {
		return review(ReasonInvalidMessage)
	}
	if in.Visibility == VisibilityPublic && !in.NoSignature {
		signed, err := s.signed(ctx, scope.Principal, in.Body)
		if err != nil {
			return nil, "", "", err
		}
		if s.checkSigned(in.Body, signed) != nil {
			return review(ReasonInvalidMessage)
		}
	}
	if _, err := s.checkAttachmentsFor(ctx, scope, Uploader{Kind: string(access.KindUser), Id: m.AuthorId}, m.AttachmentIds, m.Id); err != nil {
		if fault.As(err).Kind == fault.KindValidation {
			return cancel(ReasonAttachmentsGone)
		}
		return nil, "", "", err
	}
	if m.ThenStatusId != "" {
		set, err := s.statusSet(ctx, scope)
		if err != nil {
			return nil, "", "", err
		}
		action, target, err := thenTransition(set, m.ThenStatusId)
		if err != nil {
			return review(ReasonStatusNotAllowed)
		}
		if _, err := PlanTransition(t, action, target, set, false); err != nil {
			return review(ReasonStatusNotAllowed)
		}
	}
	return scope, "", "", nil
}
