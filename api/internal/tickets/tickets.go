// Package tickets implements the support case core: group-first ticket
// creation, atomic claim/release/transfer, lifecycle transitions with
// archive semantics, watchers, audience-separated messages,
// private drafts and staged attachments.
//
// Authorization is decided here from the request's access.ProjectScope;
// repositories enforce the same conditions atomically (versions, unassigned
// matches, group-membership touches) inside MongoDB transactions together
// with audit events and outbox records.
package tickets

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

// Ticket sources.
const (
	SourceWeb = "web" // created by a signed-in agent
	SourceAPI = "api" // created by an integration
)

// Message audiences.
const (
	VisibilityPublic = "public"
	VisibilityAgent  = "agent"
)

// Message author kinds.
const (
	AuthorUser        = "user"        // staff member
	AuthorContact     = "contact"     // the requester, posted through a requester token
	AuthorIntegration = "integration" // an agent token (a bot acting as support staff)
)

// DefaultPriority is Priority 3 (normal).
const DefaultPriority = 3

// Ref is a compact named reference.
type Ref struct {
	Id   string
	Name string
}

// organisationID is the ticket's organisation ID ("" when none).
func (t Ticket) organisationID() string {
	if t.Organisation == nil {
		return ""
	}
	return t.Organisation.Id
}

// orgIDPtr is the ticket's organisation ID for a copy (nil when none).
func orgIDPtr(t Ticket) *string {
	if id := t.organisationID(); id != "" {
		return &id
	}
	return nil
}

// OrgRef identifies a ticket's organisation.
type OrgRef struct {
	Id          string
	Name        string
	ExternalRef string
	// Active is false for a deactivated organisation (only filled when resolved for linking).
	Active bool
}

// ContactRef identifies the requester.
type ContactRef struct {
	Id         string
	Name       string
	Email      string
	ExternalId string
}

// Ticket is a support case. Names in references are resolved at read time.
type Ticket struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	Number      int64
	Subject     string
	Requester   ContactRef
	// Organisation is the organisation the creator set explicitly (nil when
	// none); it is never derived from the requester.
	Organisation *OrgRef
	// CCEmails are addresses copied on every requester email (normalized
	// lower case, unique, at most MaxCCEmails; never the requester).
	CCEmails          []string
	Group             Ref
	Assignee          *Ref
	Priority          int
	RequestedPriority int // 0 when none was requested
	Status            Ref
	StatusCategory    statuses.Category
	RequestType       Ref
	Source            string
	// CreatedBy is who created the ticket (a user, or the integration of
	// an API-created ticket); only that integration may change the subject.
	CreatedBy    audit.Actor
	WatcherCount int
	Watching     bool // for the reading user
	LastPublicId string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// PublicUpdatedAt changes only with requester-visible changes (public
	// messages, status, subject, priority, request type).
	PublicUpdatedAt  time.Time
	PublicActivityAt time.Time // public messages only
	StaffActivityAt  time.Time // any message
	ResolvedAt       *time.Time
	ArchivedAt       *time.Time
	Version          int64
	// Tags are normalized, unique and sorted (staff metadata).
	Tags []string
	// FirstResponseAt is the first public reply by a staff member.
	FirstResponseAt *time.Time
	// SLA is the current SLA cycle (nil when the project has no policy or
	// the ticket predates it); SLAView is its state at read time.
	SLA     *sla.Instance
	SLAView *sla.Summary
	// Automation is the lifecycle automation bookkeeping (staff data).
	Automation AutomationState
	// Hold is set while the ticket is On hold (nil otherwise).
	Hold *Hold
}

// Hold is the on-hold state of a ticket: the reason (its name is copied, so
// deleting the reason later loses nothing) and when the ticket returns to
// Open on its own.
type Hold struct {
	ReasonId   string
	ReasonName string
	Until      time.Time
}

// Ref is the ticket's human reference, "#" and the number (for example
// "#1042"); tickets are identified by number (owner decision 2026-10-31).
func (t Ticket) Ref() string { return "#" + strconv.FormatInt(t.Number, 10) }

// AutomationState tracks lifecycle automation timers on a ticket. The
// generation changes with every status change and every public reply, so
// a timer job scheduled for an older generation does nothing.
type AutomationState struct {
	Generation int64
	// WaitingSince is when the ticket started waiting for the requester
	// (entering waitingCustomer, or the last public staff reply while
	// waiting); nil when it is not waiting.
	WaitingSince   *time.Time
	Reminders      int
	LastReminderAt *time.Time
	// NoticeGiven is true once a reminder of this waiting period told the
	// requester that the ticket will be resolved automatically.
	NoticeGiven bool
}

// AutomationChange resets the automation timers: the generation is
// incremented, the reminder count cleared and WaitingSince replaced.
type AutomationChange struct {
	WaitingSince *time.Time
}

// AssigneeID returns the assignee's ID or "".
func (t Ticket) AssigneeID() string {
	if t.Assignee == nil {
		return ""
	}
	return t.Assignee.Id
}

// Archived reports whether the ticket is frozen.
func (t Ticket) Archived() bool { return t.StatusCategory == statuses.CategoryArchived }

// Author identifies who wrote a message.
type Author struct {
	Kind string // AuthorUser, AuthorContact or AuthorIntegration
	Id   string
	Name string
	// ViaID is the integration that posted a contact message.
	ViaId string
}

// Staff reports whether the author is support staff (a user or an agent
// token), not the requester.
func (a Author) Staff() bool { return a.Kind != AuthorContact }

// Context is the author's audience-facing kind in API and webhook payloads:
// "staff" (a user), "bot" (an agent token) or "contact" (the requester).
func (a Author) Context() string {
	switch a.Kind {
	case AuthorContact:
		return "contact"
	case AuthorIntegration:
		return "bot"
	}
	return "staff"
}

// Message is an immutable conversation entry.
type Message struct {
	Id          string
	TicketId    string
	ProjectId   string
	Visibility  string
	Author      Author
	Body        string // Markdown source; rendered safely by clients
	Attachments []Attachment
	// Seq orders a ticket's messages in commit order (it is the ticket
	// version the message committed with), so cursors never skip one.
	Seq       int64
	CreatedAt time.Time
	// Redacted is set when an administrator redacted the body.
	Redacted *Redaction
}

// RedactedBody replaces the body of a redacted message.
const RedactedBody = "[redacted]"

// Redaction records who redacted a message body, when and why.
type Redaction struct {
	At        time.Time
	ActorType string
	ActorId   string
	Reason    string
}

// Attachment states.
const (
	AttachmentUploading = "uploading"
	AttachmentStaged    = "staged"
	AttachmentAttached  = "attached"
	// AttachmentRemoved: a redaction removed the file from its message
	// (owner-approved follow-up P-DATA-1). The record stays as a placeholder
	// without name, type, size or hash; downloads answer 410 and the job
	// KindDeleteRemovedFiles deletes the bytes after the commit.
	AttachmentRemoved = "removed"
)

// Attachment is uploaded file metadata. Bytes live in GridFS.
type Attachment struct {
	Id           string
	WorkspaceId  string
	ProjectId    string
	UploaderKind string // access.Kind
	UploaderId   string
	Filename     string
	ContentType  string
	Size         int64
	SHA256       string
	State        string
	TicketId     string
	MessageId    string
	Visibility   string
	CreatedAt    time.Time
	ExpiresAt    time.Time // staged uploads only
	// ScheduledID is set while a staged upload is reserved by a scheduled
	// message: it does not expire until the message is sent or cancelled.
	ScheduledId string
}

// Removed reports whether a redaction removed the file (a placeholder that
// carries only its ID).
func (a Attachment) Removed() bool { return a.State == AttachmentRemoved }

// Draft is a private, per-author unsent message.
type Draft struct {
	TicketId   string
	AuthorId   string
	Visibility string
	Body       string
	// BaseLastPublicID is the newest public message the author had seen.
	BaseLastPublicId string
	UpdatedAt        time.Time
	Version          int64
}

// Repository errors.
var (
	ErrNotFound             = errors.New("tickets: not found")
	ErrPrecondition         = errors.New("tickets: ticket changed (precondition failed)")
	ErrIneligible           = errors.New("tickets: assignee is not an active member of the group")
	ErrAttachmentsGone      = errors.New("tickets: attachments are no longer available")
	ErrDraftConflict        = errors.New("tickets: draft version conflict")
	ErrAttachmentTooBig     = errors.New("tickets: attachment too large")
	ErrAlreadyWatching      = errors.New("tickets: already watching")
	ErrTicketArchived       = errors.New("tickets: ticket archived")
	ErrGroupUnavailable     = errors.New("tickets: group deleted or inactive")
	ErrLinkExists           = errors.New("tickets: link already exists")
	ErrTagExists            = errors.New("tickets: tag already exists")
	ErrTagNotFound          = errors.New("tickets: tag not found")
	ErrTagLimit             = errors.New("tickets: tag catalog full")
	ErrTooManyLinks         = errors.New("tickets: too many links")
	ErrAlreadyRedacted      = errors.New("tickets: message already redacted")
	ErrHoldReasonGone       = errors.New("tickets: hold reason no longer exists")
	ErrOrganisationGone     = errors.New("tickets: organisation does not exist")
	ErrOrganisationInactive = errors.New("tickets: organisation is inactive")
)

// Access restricts a query to what the reader may see.
type Access struct {
	All      bool     // every ticket in the project
	GroupIds []string // otherwise: tickets in these groups ...
	UserId   string   // ... or assigned to this user
}

// Query is an authorized ticket list query.
type Query struct {
	ProjectId   string
	ViewerId    string // for the watching flag; empty for integrations
	Access      Access
	Categories  []statuses.Category // empty = any
	AssigneeId  string
	Unassigned  bool
	GroupIds    []string // restrict to these groups (nil = no restriction)
	WatchedBy   string
	GroupFilter string
	Search      Search
	// Milestone 6 filters (saved views and list parameters); each is
	// intersected with Access.
	Priorities      []int
	StatusIds       []string
	Tags            []string // tickets carrying every tag
	RequestTypeIds  []string
	OrganisationIds []string
	// WarnBefore selects tickets whose earliest SLA warning time has
	// passed (due soon or breached), the dueSoon view.
	WarnBefore *time.Time
	// SLA narrows by the SLA state at Now (dueSoon, breached, met, missed,
	// running).
	SLA string
	// None marks a query no ticket can match (conflicting view and filter).
	None bool
	Now  time.Time
	// PublicSort orders by requester-visible updates (integrations).
	PublicSort bool
	After      *pagination.Cursor
	Limit      int
}

// Search is a parsed q parameter.
type Search struct {
	Number  int64  // exact ticket number (from "#1042" or digits)
	Subject string // case-insensitive substring
}

// Changes are the fields a mutation sets. Nil pointers are unchanged.
type Changes struct {
	Subject *string
	// OrganisationId set to "" clears the organisation.
	OrganisationId *string
	Priority       *int
	RequestTypeId  *string
	GroupId        *string
	// AssigneeID set to "" clears the assignee.
	AssigneeId       *string
	StatusId         *string
	StatusCategory   *statuses.Category
	ResolvedAt       **time.Time
	ArchivedAt       **time.Time
	PublicActivityAt *time.Time
	StaffActivityAt  *time.Time
	PublicUpdatedAt  *time.Time
	// SetLastPublic stores the inserted message's ID as the newest public
	// message.
	SetLastPublic bool
	UpdatedAt     time.Time
	// Tags replaces the tag set.
	Tags *[]string
	// CCEmails replaces the CC addresses.
	CCEmails        *[]string
	FirstResponseAt *time.Time
	// Automation, when set, resets the lifecycle automation timers.
	Automation *AutomationChange
	// Hold puts the ticket on hold (and schedules its expiry job in the
	// same transaction); ClearHold ends a hold (and cancels the job).
	Hold      *Hold
	ClearHold bool
}

// SLAUpdate computes the SLA change inside the ticket transaction from the
// ticket's current cycle (nil when it has none). It must be a pure function
// of its input because the transaction may run more than once.
type SLAUpdate func(cur *sla.Instance) SLAWrite

// SLAWrite is the outcome of an SLAUpdate: Update rewrites the current
// cycle (matched on its revision); nil changes nothing. New cycles start
// only with a new ticket or the backfill (never on reopen).
type SLAWrite struct {
	Update *sla.Instance
}

// Eligibility asks the repository to prove, inside the transaction, that
// UserID is an active user with a membership in GroupID. The membership
// record is written so a concurrent removal conflicts with the assignment.
type Eligibility struct {
	GroupId string
	UserId  string
}

// Mutation is one atomic ticket change: conditional ticket update, optional
// message insert with attachments, draft removal, audit and outbox.
type Mutation struct {
	ProjectId         string
	TicketId          string
	ExpectedVersion   int64
	RequireUnassigned bool
	// RequireAssignee matches only while this user is still the assignee.
	RequireAssignee string
	// RequireCategories restricts the current status category.
	RequireCategories []statuses.Category
	Eligible          *Eligibility
	Changes           Changes
	Message           *Message
	AttachmentIds     []string
	Uploader          Uploader
	// ClearDraftOf removes this author's draft for the message's audience.
	ClearDraftOf string
	Audit        []audit.Event
	Outbox       []outbox.Event
	ViewerId     string
	// Idempotency, when set, records "message/<id>" on the reservation in
	// the same transaction.
	Idempotency *idempotency.Ref
	// SLA, when set, updates the ticket's SLA cycle in the transaction.
	SLA SLAUpdate
	// RequireAutomationGen matches only while the automation generation is
	// unchanged (automation jobs).
	RequireAutomationGen *int64
	// ScheduledID marks the message as the delivery of a scheduled message:
	// it is stored on the message (unique, so a scheduled message can never
	// produce two messages) and its reserved attachments may be attached.
	ScheduledId string
}

// Uploader identifies who staged attachments; only they may attach them.
type Uploader struct {
	Kind string
	Id   string
}

// CreateCommand creates a ticket and its first public message.
type CreateCommand struct {
	Ticket Ticket
	// KeepOrganisation lets a copy or follow-up keep an organisation deactivated since.
	KeepOrganisation bool
	RequesterId      string
	GroupId          string
	StatusId         string
	RequestTypeId    string
	CreatedBy        audit.Actor
	Message          Message
	AttachmentIds    []string
	Uploader         Uploader
	Audit            []audit.Event
	Outbox           []outbox.Event
	Idempotency      *idempotency.Ref
	ViewerId         string
	// SLA is the first cycle (TicketID is filled by the repository).
	SLA *sla.Instance
	// Link, when set, links the new ticket to an existing one in the same
	// transaction (follow-up tickets). SourceID is filled by the
	// repository.
	Link *Link
	// LinkAudit and LinkOutbox are recorded on the linked (target) ticket.
	LinkAudit  []audit.Event
	LinkOutbox []outbox.Event
}

// TagCount is a catalog tag and how many live tickets the reader may see
// carry it.
type TagCount struct {
	Tag       string
	Count     int64
	CreatedAt time.Time
}

// Watcher is a staff user watching a ticket.
type Watcher struct {
	Id       string
	Name     string
	Username string
}

// Repository persists tickets, messages, watchers and drafts.
type Repository interface {
	ScheduledRepository

	Create(ctx context.Context, cmd CreateCommand) (Ticket, error)
	// Get finds a ticket by ID within the project.
	Get(ctx context.Context, projectID, id, viewerID string) (Ticket, error)
	// GetByNumber finds a ticket by number within the project.
	GetByNumber(ctx context.Context, projectID string, number int64, viewerID string) (Ticket, error)
	List(ctx context.Context, q Query) ([]Ticket, error)
	Count(ctx context.Context, q Query) (int64, error)
	Mutate(ctx context.Context, m Mutation) (Ticket, Message, error)
	// HoldReason returns one hold reason of the project (holdreasons.ErrNotFound
	// when there is none).
	HoldReason(ctx context.Context, projectID, id string) (holdreasons.Reason, error)
	// ExpiredHolds lists tickets still On hold whose holdUntil is before the given time.
	ExpiredHolds(ctx context.Context, before time.Time, limit int) ([]Ticket, error)
	// AssignedTickets lists live, non-archived tickets assigned to userID
	// (in projectID when it is not empty), oldest first, after the given ID.
	AssignedTickets(ctx context.Context, workspaceID, projectID, userID, afterID string, limit int) ([]Ticket, error)
	// IsEligible reports whether userID is an active user in groupID.
	IsEligible(ctx context.Context, projectID, groupID, userID string) (bool, error)
	ActiveGroup(ctx context.Context, projectID, groupID string) (Ref, bool, error)
	// Organisation resolves a project organisation (ErrNotFound when absent).
	Organisation(ctx context.Context, projectID, id string) (OrgRef, error)
	DefaultGroup(ctx context.Context, projectID string) (Ref, error)

	SetWatch(ctx context.Context, t Ticket, userID string, watch bool, ev audit.Event) (Ticket, error)
	// TicketWatchers lists a ticket's watchers (oldest first, at most limit)
	// without checking their access.
	TicketWatchers(ctx context.Context, ticketID string, limit int) ([]Watcher, error)

	ListMessages(ctx context.Context, ticketID string, visibilities []string, after *pagination.Cursor, limit int) ([]Message, error)

	GetDraft(ctx context.Context, ticketID, authorID, visibility string) (Draft, error)
	PutDraft(ctx context.Context, d Draft, expectedVersion int64) (Draft, error)
	DeleteDraft(ctx context.Context, ticketID, authorID, visibility string) error

	StagedAttachments(ctx context.Context, projectID string, ids []string) ([]Attachment, error)

	// IdempotentResource returns the resource ("ticket/<id>", "message/<id>")
	// a committed command recorded on the reservation, or "".
	IdempotentResource(ctx context.Context, ref idempotency.Ref) (string, error)
	GetMessage(ctx context.Context, ticketID, id string) (Message, error)

	// TagCatalog returns the project's catalog tags by name, each with the
	// number of live tickets acc may read that carry it.
	TagCatalog(ctx context.Context, projectID string, acc Access) ([]TagCount, error)
	// UnknownTags returns those of names that are not in the catalog.
	UnknownTags(ctx context.Context, projectID string, names []string) ([]string, error)
	// CreateTag adds a catalog tag with its audit event (ErrTagExists,
	// ErrTagLimit).
	CreateTag(ctx context.Context, workspaceID, projectID, name, createdBy string, now time.Time, ev audit.Event) error
	// DeleteTag removes a catalog tag and pulls it from every ticket of the
	// project (bumping their versions), with its audit event, in one
	// transaction. It returns how many tickets carried it (ErrTagNotFound).
	DeleteTag(ctx context.Context, workspaceID, projectID, name string, ev audit.Event) (int64, error)
	// SLACycles returns every SLA cycle of a ticket, oldest first.
	SLACycles(ctx context.Context, ticketID string) ([]sla.Instance, error)
	// StartSLA stores a first cycle for a ticket that has none (backfill);
	// it reports false when a current cycle already exists.
	StartSLA(ctx context.Context, t Ticket, inst sla.Instance) (bool, error)
	// SLABackfillCandidates lists live open/active/waiting tickets without
	// a current SLA cycle, by ID, after afterID.
	SLABackfillCandidates(ctx context.Context, projectID, afterID string, limit int) ([]Ticket, error)
	// LastSLACycle returns the ticket's latest SLA cycle, or nil.
	LastSLACycle(ctx context.Context, ticketID string) (*sla.Instance, error)
	// CheckSLA runs a timer job's check inside the caller's (fenced)
	// transaction: when the cycle still has the given generation it records
	// breaches, emits warning/breach outbox events and schedules the next
	// check. When SLA was turned off after the cycle started (inactive or
	// since deactivated policy) it stops the cycle and announces nothing.
	CheckSLA(ctx context.Context, instanceID string, generation int64, now time.Time) error
	// StopSLA runs one part of a deactivation sweep inside the caller's
	// (fenced) transaction: it stops up to limit of the project's open
	// cycles that started with an older policy version, clears their
	// tickets' due-soon projection and enqueues the next part while more
	// remain (the last part records the audit event).
	StopSLA(ctx context.Context, r sla.StopRun, limit int, now time.Time) error
	// RecordAudit stores audit events outside a ticket mutation.
	RecordAudit(ctx context.Context, workspaceID string, events ...audit.Event) error

	// Search returns the authorized tickets matching a full-text query,
	// best first (score, then ID, descending), and whether a candidate
	// bound was reached.
	Search(ctx context.Context, q SearchQuery) ([]SearchCandidate, bool, error)
	// ListByIDs returns the project's tickets with these IDs.
	ListByIDs(ctx context.Context, projectID string, ids []string, viewerID string) ([]Ticket, error)
	// MessageBodies returns messages (without attachments) by ID.
	MessageBodies(ctx context.Context, projectID string, ids []string) (map[string]Message, error)

	// Links (links.go).
	InsertLink(ctx context.Context, l Link, events []audit.Event, ob []outbox.Event) (Link, error)
	DeleteLink(ctx context.Context, l Link, events []audit.Event, ob []outbox.Event) error
	GetLink(ctx context.Context, projectID, id string) (Link, error)
	TicketLinks(ctx context.Context, ticketID string, limit int) ([]Link, error)

	// TicketActivity pages through a ticket's audit events (ticket.* and
	// message.* actions), oldest first.
	TicketActivity(ctx context.Context, projectID, ticketID string, f ActivityFilter, after *pagination.Cursor, limit int) ([]ActivityEvent, error)

	// RedactMessage replaces a message body and scrubs derived copies in
	// one transaction.
	RedactMessage(ctx context.Context, cmd RedactCommand) (Ticket, Message, error)

	// RemindRequester records a follow-up reminder when the ticket is still
	// waiting for the requester at the given automation generation and
	// reminder count; it reports whether the reminder was recorded.
	RemindRequester(ctx context.Context, cmd ReminderCommand) (bool, error)
}

// FileStore stores attachment bytes (GridFS) and their metadata records.
type FileStore interface {
	// Begin records an upload in state "uploading" before any bytes exist.
	Begin(ctx context.Context, a Attachment) (Attachment, error)
	// Write streams bytes for a begun upload; it stops after limit bytes and
	// returns ErrAttachmentTooBig (the partial file is removed).
	Write(ctx context.Context, a Attachment, r io.Reader, limit int64) (size int64, sha string, err error)
	// Stage marks the upload complete and claimable until expiresAt and
	// appends the audit event in the same transaction.
	Stage(ctx context.Context, a Attachment, ev audit.Event) (Attachment, error)
	// Discard removes a failed upload's bytes and record.
	Discard(ctx context.Context, a Attachment) error
	Get(ctx context.Context, projectID, id string) (Attachment, error)
	Open(ctx context.Context, a Attachment) (io.ReadCloser, error)
	// CleanupExpired removes expired unattached uploads; it returns how many
	// were removed.
	CleanupExpired(ctx context.Context, now time.Time, limit int) (int, error)
	// PendingUploads counts the uploader's unexpired, not yet attached
	// uploads in the project and their staged bytes.
	PendingUploads(ctx context.Context, projectID, uploaderKind, uploaderID string, now time.Time) (count, bytes int64, err error)
	// DeleteRemovedFiles deletes the bytes of the message's removed
	// attachments that still have them and records when; it returns how
	// many files it deleted. Idempotent.
	DeleteRemovedFiles(ctx context.Context, projectID, messageID string, now time.Time) (int, error)
}
