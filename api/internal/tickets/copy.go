package tickets

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// CopyInput overrides the copied values. Every field is optional: absent
// fields take the original's value.
type CopyInput struct {
	Subject       *string
	GroupId       *string
	Priority      *int
	RequestTypeId *string
	Tags          *[]string
	// Body is the first message; the original's first public message by default.
	Body *string
	// IncludeAttachments copies the original first message's files
	// (default true).
	IncludeAttachments *bool
}

// Copy creates a new ticket with the original's requester, CC addresses,
// organisation, priority, request type, group, tags, first public message
// and (by default) the first message's attachments, linked copiedFrom the
// original in the same transaction. The original may be in any state,
// including archived. Not copied: assignee, watchers, status (the copy is
// Open), SLA (the copy starts its own first-response cycle), later
// messages, agent notes, history and holds. The first message is authored
// like any created ticket's: by the copying user, or by the bot for a token
// holding tickets:comment (or tickets:all), otherwise by the requester. It is
// the original text, and no signature is appended. Attachment bytes are duplicated so redacting or deleting one
// ticket's files never affects the other's.
func (s *Service) Copy(ctx context.Context, scope *access.ProjectScope, ref string, in CopyInput) (Ticket, error) {
	if err := scope.Require(access.CapTicketCreate); err != nil {
		return Ticket{}, err
	}
	if !scope.Principal.IsStaff() {
		return Ticket{}, fault.Forbidden("staffOnly", "Only support staff can copy tickets.")
	}
	if err := requireOverrides(scope, in.GroupId, in.RequestTypeId, in.Tags != nil && len(*in.Tags) > 0); err != nil {
		return Ticket{}, err
	}
	orig, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, err
	}
	first, err := s.repo.ListMessages(ctx, orig.Id, []string{VisibilityPublic}, nil, 1)
	if err != nil {
		return Ticket{}, err
	}
	body := "Copy of " + orig.Ref() + "."
	var files []Attachment
	if len(first) > 0 {
		if first[0].Redacted == nil && first[0].Body != "" {
			body = first[0].Body
		}
		for _, a := range first[0].Attachments {
			if a.State == AttachmentAttached {
				files = append(files, a)
			}
		}
	}
	if in.Body != nil {
		body = *in.Body
	}
	subject := orig.Subject
	if in.Subject != nil {
		subject = *in.Subject
	}
	contactID := orig.Requester.Id
	cin := CreateInput{Subject: subject, Body: body, Priority: in.Priority, GroupId: in.GroupId, RequestTypeId: in.RequestTypeId,
		Requester: RequesterInput{ContactId: &contactID}, OrganisationId: orgIDPtr(orig), keepOrganisation: true, CCEmails: orig.CCEmails}
	if cin.Priority == nil {
		p := orig.Priority
		cin.Priority = &p
	}
	if cin.GroupId == nil {
		if _, active, err := s.repo.ActiveGroup(ctx, scope.Project.Id, orig.Group.Id); err == nil && active {
			gid := orig.Group.Id
			cin.GroupId = &gid
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return Ticket{}, err
		}
	}
	if cin.RequestTypeId == nil {
		rtID := orig.RequestType.Id
		if _, err := s.requestType(ctx, scope, &rtID); err == nil {
			cin.RequestTypeId = &rtID
		}
	}
	if in.Tags != nil {
		cin.Tags = *in.Tags
	} else if len(orig.Tags) > 0 {
		// Tags the admin removed from the catalog since are dropped.
		unknown, err := s.repo.UnknownTags(ctx, scope.Project.Id, orig.Tags)
		if err != nil {
			return Ticket{}, err
		}
		for _, t := range orig.Tags {
			if !slices.Contains(unknown, t) {
				cin.Tags = append(cin.Tags, t)
			}
		}
	}
	// A retry of a committed copy must not stage the files again.
	_, done, err := s.replayed(ctx, scope, "ticket/")
	if err != nil {
		return Ticket{}, err
	}
	copyFiles := !done && (in.IncludeAttachments == nil || *in.IncludeAttachments) && len(files) > 0
	// Everything that can be refused is checked before any bytes are copied.
	if err := s.precheckCopy(ctx, scope, cin, files, copyFiles); err != nil {
		return Ticket{}, err
	}
	var staged []Attachment
	if copyFiles {
		if staged, err = s.duplicateFiles(ctx, scope, files); err != nil {
			return Ticket{}, err
		}
		for _, a := range staged {
			cin.AttachmentIds = append(cin.AttachmentIds, a.Id)
		}
	}
	now := s.clock.Now()
	link := &Link{ProjectId: scope.Project.Id, TargetId: orig.Id, Type: LinkCopy, CreatedBy: s.actor(scope), CreatedAt: now}
	ev := audit.New(ctx, s.actor(scope), audit.TicketLinkAdded, "", now)
	ev.ProjectId, ev.TicketId, ev.Changes = scope.Project.Id, orig.Id, []string{"links:" + LinkCopy}
	ob := outbox.Event{Type: OutboxLinkAdded, ProjectId: scope.Project.Id, TicketId: orig.Id, Visibility: VisibilityAgent,
		Actor: s.actor(scope), OccurredAt: now}
	out, err := s.create(ctx, scope, cin, &followUp{link: link, copied: true, audit: []audit.Event{ev}, outbox: []outbox.Event{ob}})
	if err != nil {
		s.discardAll(ctx, staged)
		return Ticket{}, err
	}
	return out, nil
}

// precheckCopy validates the new ticket's input, group, request type, tags
// and the file limits, so a refused copy never duplicates any bytes.
func (s *Service) precheckCopy(ctx context.Context, scope *access.ProjectScope, cin CreateInput, files []Attachment, copyFiles bool) error {
	if err := cin.validate(s); err != nil {
		return err
	}
	if cin.GroupId != nil {
		_, active, err := s.repo.ActiveGroup(ctx, scope.Project.Id, *cin.GroupId)
		if errors.Is(err, ErrNotFound) {
			return fault.Field("groupId", "notFound", "No group with this ID exists in the project.")
		}
		if err != nil {
			return err
		}
		if !active {
			return fault.Field("groupId", "inactive", "The group is inactive.")
		}
	}
	if _, err := s.requestType(ctx, scope, cin.RequestTypeId); err != nil {
		return err
	}
	if len(cin.Tags) > 0 {
		var tv validation.Collector
		tags := NormalizeTags(&tv, "tags", cin.Tags)
		if err := tv.Err(); err != nil {
			return err
		}
		if err := s.checkTagsKnown(ctx, scope.Project.Id, cin.Tags, tags, nil); err != nil {
			return err
		}
	}
	if !copyFiles {
		return nil
	}
	lim := s.limits()
	if int64(len(files)) > lim.MaxAttachmentsPerMessage {
		return fault.Field("attachmentIds", validation.CodeOutOfRange,
			"Attach at most "+strconv.FormatInt(lim.MaxAttachmentsPerMessage, 10)+" files to one message.")
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if total > lim.MaxMessageTotalBytes {
		return fault.Field("attachmentIds", "tooLarge", "The attachments exceed the per-message size limit.")
	}
	p := scope.Principal
	count, bytes, err := s.files.PendingUploads(ctx, scope.Project.Id, string(p.Kind), p.Id, s.clock.Now())
	if err != nil {
		return err
	}
	if count+int64(len(files)) > MaxPendingUploads || bytes+total > maxPendingUploadSizes*lim.MaxAttachmentBytes {
		return fault.RateLimited("tooManyPendingUploads",
			"You have too many uploaded files that are not attached to a message yet. Attach or let them expire, then try again.", time.Hour)
	}
	return nil
}

// discardAll removes staged copies that no ticket will use.
func (s *Service) discardAll(ctx context.Context, staged []Attachment) {
	for _, a := range staged {
		_ = s.files.Discard(context.WithoutCancel(ctx), a)
	}
}

// duplicateFiles stages a copy of each attachment as the caller's upload
// (outside any transaction, like Upload) and returns them. On any failure
// the copies made so far are discarded.
func (s *Service) duplicateFiles(ctx context.Context, scope *access.ProjectScope, files []Attachment) (staged []Attachment, err error) {
	defer func() {
		if err != nil {
			s.discardAll(ctx, staged)
			staged = nil
		}
	}()
	p := scope.Principal
	limit := s.limits().MaxAttachmentBytes
	for _, src := range files {
		rc, oerr := s.files.Open(ctx, src)
		if errors.Is(oerr, ErrNotFound) {
			return staged, errAttachmentNotFound()
		}
		if oerr != nil {
			return staged, oerr
		}
		now := s.clock.Now()
		a, berr := s.files.Begin(ctx, Attachment{WorkspaceId: p.WorkspaceId, ProjectId: scope.Project.Id,
			UploaderKind: string(p.Kind), UploaderId: p.Id, Filename: src.Filename, ContentType: src.ContentType,
			State: AttachmentUploading, CreatedAt: now, ExpiresAt: now.Add(StagingTTL)})
		if berr != nil {
			_ = rc.Close()
			return staged, berr
		}
		size, sum, werr := s.files.Write(ctx, a, rc, limit)
		_ = rc.Close()
		if werr != nil {
			_ = s.files.Discard(context.WithoutCancel(ctx), a)
			if errors.Is(werr, ErrAttachmentTooBig) {
				return staged, fault.New(fault.KindTooLarge, "attachmentTooLarge",
					"An attachment is larger than the limit of "+strconv.FormatInt(limit, 10)+" bytes.")
			}
			return staged, werr
		}
		a.Size, a.SHA256, a.State = size, sum, AttachmentStaged
		ev := audit.New(ctx, s.actor(scope), audit.AttachmentUploaded, "", s.clock.Now())
		ev.ProjectId, ev.Changes = scope.Project.Id, []string{"size", "contentType"}
		out, serr := s.files.Stage(ctx, a, ev)
		if errors.Is(serr, ErrNotFound) {
			_ = s.files.Discard(context.WithoutCancel(ctx), a)
			return staged, fault.Conflict("uploadExpired", "The upload took too long and was discarded. Try again.")
		}
		if serr != nil {
			return staged, serr
		}
		staged = append(staged, out)
	}
	return staged, nil
}
