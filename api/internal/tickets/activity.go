package tickets

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// ActivityActor is the staff-safe actor of an activity entry: the type,
// the user or integration ID and a display name (user display name,
// integration name or the system/operator label).
type ActivityActor struct {
	Type string
	Id   string
	Name string
	// Scopes are the API token scopes that allowed the action (tokens).
	Scopes []string
}

// ActivityEvent is one entry of a ticket's activity timeline: an audit
// event projected for staff. Audit events never contain message bodies,
// secrets or field values — only field names, identifiers and reasons.
type ActivityEvent struct {
	Id        string
	Action    string
	Actor     ActivityActor
	Target    string
	Changes   []string
	Reason    string
	Outcome   string
	CreatedAt time.Time
}

// activityPrefixes are the audit actions shown in the timeline. Other
// audit actions recorded against a ticket (e.g. mcp.toolCalled) stay in
// the audit log.
var activityPrefixes = []string{"ticket.", "message."}

// ActivityAction reports whether an audit action belongs in the timeline.
func ActivityAction(action string) bool {
	for _, p := range activityPrefixes {
		if strings.HasPrefix(action, p) {
			return true
		}
	}
	return false
}

// ActivityFilter orders and narrows a ticket's activity.
type ActivityFilter struct {
	NewestFirst bool
	// Action keeps one exact action (e.g. message.redacted); empty = all.
	Action string
}

// ListActivity pages through the activity of a live ticket the caller can
// read, oldest first (or newest first). It is staff-only: integrations receive 403
// userRequired because the timeline reveals staff identities and agent-note
// activity (the requester-side projection hides both).
func (s *Service) ListActivity(ctx context.Context, scope *access.ProjectScope, ref string, f ActivityFilter, after *pagination.Cursor, limit int) (pagination.Page[ActivityEvent], error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return pagination.Page[ActivityEvent]{}, err
	}
	if f.Action != "" && !ActivityAction(f.Action) {
		return pagination.Page[ActivityEvent]{}, fault.Field("action", "invalidChoice", "Use a ticket.* or message.* action.")
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return pagination.Page[ActivityEvent]{}, err
	}
	rows, err := s.repo.TicketActivity(ctx, scope.Project.Id, t.Id, f, after, limit+1)
	if err != nil {
		return pagination.Page[ActivityEvent]{}, err
	}
	return pagination.NewPage(rows, limit, func(e ActivityEvent) pagination.Cursor {
		return pagination.Cursor{Sort: e.CreatedAt.UTC().Format(time.RFC3339Nano), Id: e.Id}
	}), nil
}

// ---- Redaction ----

// KindDeleteRemovedFiles is the job that deletes the GridFS bytes of the
// attachments a redaction removed (payload: projectId, messageId). It is
// enqueued in the redaction transaction, so a crash between the commit and
// the deletion loses nothing; failed attempts are retried with the job
// backoff.
const KindDeleteRemovedFiles = "attachments.deleteFiles"

// RedactInput is a redaction request.
type RedactInput struct {
	Reason string
	// RemoveAttachments also removes the message's attachments (owner-
	// approved follow-up P-DATA-1). On a message whose body was already
	// redacted it removes the attachments that were kept then.
	RemoveAttachments bool
}

// RedactCommand is the repository's redaction transaction.
type RedactCommand struct {
	ProjectId string
	TicketId  string
	MessageId string
	Redaction Redaction
	// KeepBody: the body was redacted before; only attachments are removed
	// (the message's redaction record stays as it is).
	KeepBody bool
	// RemoveAttachments lists the attachments to remove; every one must
	// still be attached to the message (else ErrAlreadyRedacted).
	RemoveAttachments []string
	// FilesJob deletes the removed attachments' bytes after the commit
	// (enqueued in the same transaction).
	FilesJob *jobs.NewJob
	// Idempotency is the request's reserved Idempotency-Key record; it is
	// linked to the message, so a later redaction drops its stored response
	// (which may still name attachments removed then).
	Idempotency *idempotency.Ref
	Audit       []audit.Event
	Outbox      []outbox.Event
}

func errAlreadyRedacted() error {
	return fault.Conflict("messageAlreadyRedacted", "This message was already redacted.")
}

// RedactMessage replaces a sent message's body with "[redacted]"
// (architecture §4.1 privacy redaction). It records who, when and why on
// the message and in the audit log (never the old body), moves the ticket
// version so cached views bound to the old content are invalidated, and
// in the same transaction scrubs derived copies: the full-text index
// follows the new body and unsent webhook payloads are rebuilt from the
// current state. Drafts are not touched. Copies already delivered by email or
// webhook cannot be retracted. Admins only (message.redact); allowed
// on archived tickets.
//
// With RemoveAttachments the message's attachments become placeholders in
// the same transaction (state removed; name, type, size and hash dropped;
// who, when and why recorded) and are no longer served; their bytes are
// deleted afterwards by the durable job KindDeleteRemovedFiles. A message
// whose body was redacted earlier with its attachments kept can be
// redacted again with RemoveAttachments to remove them; otherwise a second
// redaction is messageAlreadyRedacted.
func (s *Service) RedactMessage(ctx context.Context, scope *access.ProjectScope, ref, messageID string, in RedactInput) (Message, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Message{}, err
	}
	if err := scope.Require(access.CapMessageRedact); err != nil {
		return Message{}, err
	}
	var v validation.Collector
	v.String(validation.MessageRedactReason, "reason", in.Reason)
	if err := v.Err(); err != nil {
		return Message{}, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Message{}, err
	}
	if !validation.ValidID(messageID) {
		return Message{}, errMessageNotFound()
	}
	m, err := s.repo.GetMessage(ctx, t.Id, messageID)
	if errors.Is(err, ErrNotFound) {
		return Message{}, errMessageNotFound()
	}
	if err != nil {
		return Message{}, err
	}
	var remove []string
	if in.RemoveAttachments {
		for _, a := range m.Attachments {
			if !a.Removed() {
				remove = append(remove, a.Id)
			}
		}
	}
	keepBody := m.Redacted != nil
	if keepBody && len(remove) == 0 {
		return Message{}, errAlreadyRedacted()
	}
	now := s.clock.Now()
	actor := s.actor(scope)
	ev := audit.New(ctx, actor, audit.MessageRedacted, "message/"+m.Id, now)
	ev.ProjectId, ev.TicketId, ev.Reason = scope.Project.Id, t.Id, in.Reason
	if !keepBody {
		ev.Changes = append(ev.Changes, "body")
	}
	ev.Changes = append(ev.Changes, "visibility:"+m.Visibility)
	cmd := RedactCommand{ProjectId: scope.Project.Id, TicketId: t.Id, MessageId: m.Id,
		Redaction: Redaction{At: now, ActorType: actor.Type, ActorId: scope.Principal.Id, Reason: in.Reason},
		KeepBody:  keepBody, RemoveAttachments: remove,
		Outbox: []outbox.Event{{Type: OutboxMessageRedacted, ProjectId: scope.Project.Id, TicketId: t.Id, Visibility: m.Visibility, Actor: actor, OccurredAt: now}},
	}
	if len(remove) > 0 {
		// The audit event names the removed files by ID only.
		ev.Changes = append(ev.Changes, "attachmentsRemoved:"+strconv.Itoa(len(remove)))
		for _, id := range remove {
			ev.Changes = append(ev.Changes, "attachment/"+id)
		}
		cmd.FilesJob = &jobs.NewJob{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, Kind: KindDeleteRemovedFiles,
			Key:     "attachment-files:" + m.Id + ":" + strconv.FormatInt(now.UnixMilli(), 10),
			Payload: map[string]string{"projectId": scope.Project.Id, "messageId": m.Id}}
	}
	cmd.Audit = []audit.Event{ev}
	if ref, ok := idempotency.RefFrom(ctx); ok {
		cmd.Idempotency = &ref
	}
	_, out, err := s.repo.RedactMessage(ctx, cmd)
	switch {
	case errors.Is(err, ErrAlreadyRedacted):
		return Message{}, errAlreadyRedacted()
	case errors.Is(err, ErrNotFound):
		return Message{}, errMessageNotFound()
	case err != nil:
		return Message{}, err
	}
	if cmd.FilesJob != nil {
		s.wake(ctx)
	}
	return out, nil
}

// HandleDeleteRemovedFiles is the jobs.Handler for KindDeleteRemovedFiles.
// It runs outside any transaction (GridFS deletes cannot be part of one)
// and is idempotent: files already deleted are skipped, so a retry after a
// failure or a lost lease finishes the rest. Storage errors are retried.
func (s *Service) HandleDeleteRemovedFiles(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	projectID, messageID := j.Payload["projectId"], j.Payload["messageId"]
	if !validation.ValidID(projectID) || !validation.ValidID(messageID) {
		return jobs.Result{Outcome: jobs.Dead, ErrorClass: "invalidPayload", Error: "the attachment deletion payload is invalid"}
	}
	if _, err := s.files.DeleteRemovedFiles(ctx, projectID, messageID, s.clock.Now()); err != nil {
		return jobs.Result{Outcome: jobs.Retry, ErrorClass: "storage", Error: "deleting the removed attachments' files failed: " + err.Error()}
	}
	return jobs.Result{Outcome: jobs.Done}
}

// ---- Live updates ----

// LiveVisible decides whether a committed change of ticketID (outbox type
// and audience) may be announced to scope, and with which version. Staff
// see changes of tickets they can read now; integrations see only
// public-audience changes and always
// receive version 0 (their projection never exposes the staff version).
// Access is evaluated at delivery time against the current ticket.
func (s *Service) LiveVisible(ctx context.Context, scope *access.ProjectScope, ticketID, typ, visibility string, version int64) (int64, bool, error) {
	if !scope.Can(access.CapTicketRead) {
		return 0, false, nil
	}
	if !scope.Principal.IsStaff() {
		if visibility != VisibilityPublic {
			return 0, false, nil
		}
		if _, err := s.load(ctx, scope, ticketID); err != nil {
			return 0, false, ignoreHidden(err)
		}
		return 0, true, nil
	}
	if visibility == VisibilityAgent && !scope.Can(access.CapMessageReadPrivate) {
		return 0, false, nil
	}
	if _, err := s.load(ctx, scope, ticketID); err != nil {
		return 0, false, ignoreHidden(err)
	}
	return version, true, nil
}

func ignoreHidden(err error) error {
	if fe := fault.As(err); fe.Kind == fault.KindNotFound || fe.Kind == fault.KindForbidden {
		return nil
	}
	return err
}
