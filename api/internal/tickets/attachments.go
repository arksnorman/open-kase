package tickets

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// StagingTTL is how long an uploaded but unattached file is kept.
const StagingTTL = 24 * time.Hour

// Bounds on uploads staged but not yet attached, per uploader and project.
// They keep a leaked or misbehaving credential from filling GridFS with
// files that are only removed after StagingTTL.
const (
	MaxPendingUploads = 50
	// maxPendingUploadSizes times the per-file limit is the byte bound.
	maxPendingUploadSizes = 20
)

func errAttachmentsGone() error {
	return fault.Field("attachmentIds", "notAvailable", "One or more attachments are not staged by you in this project, were already used, or expired. Upload them again.")
}

func errAttachmentNotFound() error { return fault.NotFound("attachmentNotFound") }

// SanitizeFilename reduces a client-supplied name to a safe base name:
// no directories, no control or bidi-override characters, at most 255
// characters, never empty.
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '"' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(strings.Trim(name, "."))
	if name == "" || name == "/" {
		name = "attachment"
	}
	if r := []rune(name); len(r) > 255 {
		ext := path.Ext(name)
		if len([]rune(ext)) > 20 {
			ext = ""
		}
		name = string(r[:255-len([]rune(ext))]) + ext
	}
	return name
}

// canUpload reports whether the principal may stage files: anyone who can
// write a message or create a ticket in the project. The first matching
// capability is required (so a token's audit event names its scope).
func canUpload(scope *access.ProjectScope) error {
	for _, c := range []access.Capability{access.CapTicketReplyPublic, access.CapTicketNoteAgent, access.CapTicketReplyAsRequester, access.CapTicketCreate} {
		if scope.Can(c) {
			return scope.Require(c)
		}
	}
	return scope.Require(access.CapTicketReplyPublic)
}

// Upload stages a file for a later message. Bytes are streamed to GridFS
// outside any transaction; the metadata record moves uploading → staged.
// The upload expires after StagingTTL unless a message attaches it.
func (s *Service) Upload(ctx context.Context, scope *access.ProjectScope, filename string, r io.Reader) (Attachment, error) {
	if err := canUpload(scope); err != nil {
		return Attachment{}, err
	}
	name := SanitizeFilename(filename)
	var v validation.Collector
	v.String(validation.AttachmentFilename, "file", name)
	if err := v.Err(); err != nil {
		return Attachment{}, err
	}
	limit := s.limits().MaxAttachmentBytes
	br := bufio.NewReaderSize(r, 512)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return Attachment{}, fault.Wrap(fault.KindMalformed, "uploadFailed", err)
	}
	if len(head) == 0 {
		return Attachment{}, fault.Field("file", "empty", "The file is empty.")
	}
	now := s.clock.Now()
	p := scope.Principal
	count, bytes, err := s.files.PendingUploads(ctx, scope.Project.Id, string(p.Kind), p.Id, now)
	if err != nil {
		return Attachment{}, err
	}
	if count >= MaxPendingUploads || bytes >= maxPendingUploadSizes*limit {
		return Attachment{}, fault.RateLimited("tooManyPendingUploads",
			"You have too many uploaded files that are not attached to a message yet. Attach or let them expire, then try again.", time.Hour)
	}
	a, err := s.files.Begin(ctx, Attachment{WorkspaceId: p.WorkspaceId, ProjectId: scope.Project.Id,
		UploaderKind: string(p.Kind), UploaderId: p.Id, Filename: name, ContentType: http.DetectContentType(head),
		State: AttachmentUploading, CreatedAt: now, ExpiresAt: now.Add(StagingTTL)})
	if err != nil {
		return Attachment{}, err
	}
	size, sum, err := s.files.Write(ctx, a, br, limit)
	if err != nil {
		_ = s.files.Discard(context.WithoutCancel(ctx), a)
		if errors.Is(err, ErrAttachmentTooBig) {
			return Attachment{}, fault.New(fault.KindTooLarge, "attachmentTooLarge",
				"The file is larger than the limit of "+strconv.FormatInt(limit, 10)+" bytes.")
		}
		return Attachment{}, err
	}
	a.Size, a.SHA256, a.State = size, sum, AttachmentStaged
	// The upload is audited (file metadata only, never the name or bytes):
	// every write made with an API token appears in the audit log.
	ev := audit.New(ctx, s.actor(scope), audit.AttachmentUploaded, "", s.clock.Now())
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"size", "contentType"}
	out, err := s.files.Stage(ctx, a, ev)
	if errors.Is(err, ErrNotFound) {
		return Attachment{}, fault.Conflict("uploadExpired", "The upload took too long and was discarded. Upload the file again.")
	}
	return out, err
}

// MaxUploadBytes is the configured per-file limit (for transport limits).
func (s *Service) MaxUploadBytes() int64 { return s.limits().MaxAttachmentBytes }

// checkAttachments validates attachment IDs for a new message: count and
// total size limits, and that each is a staged upload by this principal in
// this project. The repository re-checks atomically when attaching.
func (s *Service) checkAttachments(ctx context.Context, scope *access.ProjectScope, up Uploader, ids []string) ([]string, error) {
	return s.checkAttachmentsFor(ctx, scope, up, ids, "")
}

// checkAttachmentsFor is checkAttachments that also accepts uploads
// reserved by the scheduled message scheduledID (they have no expiry).
func (s *Service) checkAttachmentsFor(ctx context.Context, scope *access.ProjectScope, up Uploader, ids []string, scheduledID string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	lim := s.limits()
	if int64(len(ids)) > lim.MaxAttachmentsPerMessage {
		return nil, fault.Field("attachmentIds", validation.CodeOutOfRange,
			"Attach at most "+strconv.FormatInt(lim.MaxAttachmentsPerMessage, 10)+" files to one message.")
	}
	staged, err := s.repo.StagedAttachments(ctx, scope.Project.Id, ids)
	if err != nil {
		return nil, err
	}
	var total int64
	now := s.clock.Now()
	ok := 0
	for _, a := range staged {
		usable := a.ScheduledId == "" && a.ExpiresAt.After(now)
		if scheduledID != "" {
			usable = a.ScheduledId == scheduledID
		}
		if a.UploaderKind == up.Kind && a.UploaderId == up.Id && a.State == AttachmentStaged && usable {
			ok++
			total += a.Size
		}
	}
	if ok != len(ids) {
		return nil, errAttachmentsGone()
	}
	if total > lim.MaxMessageTotalBytes {
		return nil, fault.Field("attachmentIds", "tooLarge", "The attachments exceed the per-message size limit.")
	}
	return ids, nil
}

// Download authorizes and opens an attachment. Attached files inherit the
// audience of their message: agent-note files are invisible (404) to
// integrations. A staged file is visible only to its uploader. A file a
// redaction removed is 410 attachmentRemoved, but only for callers who
// could read it otherwise (the same checks run first, so its existence is
// not revealed to anyone else).
func (s *Service) Download(ctx context.Context, scope *access.ProjectScope, id string) (Attachment, io.ReadCloser, error) {
	if !validation.ValidID(id) {
		return Attachment{}, nil, errAttachmentNotFound()
	}
	a, err := s.files.Get(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) {
		return Attachment{}, nil, errAttachmentNotFound()
	}
	if err != nil {
		return Attachment{}, nil, err
	}
	switch a.State {
	case AttachmentStaged:
		if a.UploaderKind != string(scope.Principal.Kind) || a.UploaderId != scope.Principal.Id {
			return Attachment{}, nil, errAttachmentNotFound()
		}
	case AttachmentAttached, AttachmentRemoved:
		audiences, err := readableAudiences(scope)
		if err != nil {
			return Attachment{}, nil, err
		}
		if !slices.Contains(audiences, a.Visibility) {
			return Attachment{}, nil, errAttachmentNotFound()
		}
		if _, err := s.load(ctx, scope, a.TicketId); err != nil {
			if fault.As(err).Code == "ticketNotFound" {
				return Attachment{}, nil, errAttachmentNotFound()
			}
			return Attachment{}, nil, err
		}
		if a.Removed() {
			return Attachment{}, nil, fault.New(fault.KindGone, "attachmentRemoved",
				"This attachment was removed by a administrator and can no longer be downloaded.")
		}
	default:
		return Attachment{}, nil, errAttachmentNotFound()
	}
	rc, err := s.files.Open(ctx, a)
	if errors.Is(err, ErrNotFound) {
		return Attachment{}, nil, errAttachmentNotFound()
	}
	return a, rc, err
}

// CleanupExpiredUploads removes expired staged uploads. It is safe to run
// concurrently from several processes.
func (s *Service) CleanupExpiredUploads(ctx context.Context) (int, error) {
	return s.files.CleanupExpired(ctx, s.clock.Now(), 200)
}
