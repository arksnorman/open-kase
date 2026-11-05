package http

import (
	"time"

	"github.com/arksnorman/open-kase/api/internal/tickets"
)

type contactRefDTO struct {
	Id         string  `json:"id"`
	Name       string  `json:"name"`
	Email      string  `json:"email"`
	ExternalId *string `json:"externalId"`
}

type statusRefDTO struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category" enum:"open,waitingCustomer,onHold,resolved,archived"`
}

// ticketDTO is the contract's Ticket.
type ticketDTO struct {
	Id                  string              `json:"id"`
	Number              int64               `json:"number" doc:"The ticket number, shown as #1042. Use it (or the ID) in /tickets/{t}."`
	ProjectId           string              `json:"projectId"`
	Subject             string              `json:"subject"`
	Requester           contactRefDTO       `json:"requester"`
	Organisation        *organisationRefDTO `json:"organisation" doc:"The organisation the creator linked to the ticket (never derived from the requester); null when none."`
	CCEmails            []string            `json:"ccEmails" doc:"Addresses copied on every email to the requester (lower case, at most 10; empty when none)."`
	Group               refDTO              `json:"group"`
	Source              string              `json:"source" enum:"web,api"`
	Assignee            *refDTO             `json:"assignee"`
	Priority            int                 `json:"priority" minimum:"1" maximum:"4" doc:"1 critical, 2 high, 3 normal, 4 low."`
	RequestedPriority   *int                `json:"requestedPriority"`
	Status              statusRefDTO        `json:"status"`
	RequestType         refDTO              `json:"requestType"`
	Watching            bool                `json:"watching"`
	WatcherCount        int                 `json:"watcherCount"`
	LastPublicMessageId *string             `json:"lastPublicMessageId"`
	CreatedAt           time.Time           `json:"createdAt"`
	UpdatedAt           time.Time           `json:"updatedAt"`
	ActivityAt          time.Time           `json:"activityAt"`
	ResolvedAt          *time.Time          `json:"resolvedAt"`
	ArchivedAt          *time.Time          `json:"archivedAt"`
	Version             int64               `json:"version" doc:"Send as If-Match on mutations. Integrations receive 0."`
	// Milestone 6 (staff only; integrations receive empty values).
	Tags            []string   `json:"tags"`
	FirstResponseAt *time.Time `json:"firstResponseAt"`
	SLA             *slaDTO    `json:"sla"`
	Hold            *holdDTO   `json:"hold" doc:"Staff only: set while the ticket is On hold (the reason, copied when the hold started, and when it comes back to Open); null otherwise."`
}

type holdDTO struct {
	ReasonId   string    `json:"reasonId"`
	ReasonName string    `json:"reasonName"`
	Until      time.Time `json:"until"`
}

func toRef(r *tickets.Ref) *refDTO {
	if r == nil {
		return nil
	}
	return &refDTO{Id: r.Id, Name: r.Name}
}

func toTicket(t tickets.Ticket) ticketDTO {
	out := ticketDTO{
		Id: t.Id, Number: t.Number, ProjectId: t.ProjectId, Subject: t.Subject, Organisation: toOrganisationRef(t.Organisation),
		Requester: contactRefDTO{Id: t.Requester.Id, Name: t.Requester.Name, Email: t.Requester.Email, ExternalId: nullable(t.Requester.ExternalId)},
		Group:     refDTO{Id: t.Group.Id, Name: t.Group.Name}, Source: t.Source,
		Assignee: toRef(t.Assignee), Priority: t.Priority,
		Status:      statusRefDTO{Id: t.Status.Id, Name: t.Status.Name, Category: string(t.StatusCategory)},
		RequestType: refDTO{Id: t.RequestType.Id, Name: t.RequestType.Name}, Watching: t.Watching, WatcherCount: t.WatcherCount,
		LastPublicMessageId: nullable(t.LastPublicId), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, ActivityAt: t.StaffActivityAt,
		ResolvedAt: t.ResolvedAt, ArchivedAt: t.ArchivedAt, Version: t.Version,
	}
	if t.RequestedPriority > 0 {
		rp := t.RequestedPriority
		out.RequestedPriority = &rp
	}
	out.Tags, out.FirstResponseAt, out.SLA = nonNil(t.Tags), t.FirstResponseAt, toSLA(t.SLAView)
	out.CCEmails = nonNil(t.CCEmails)
	if t.Hold != nil {
		out.Hold = &holdDTO{ReasonId: t.Hold.ReasonId, ReasonName: t.Hold.ReasonName, Until: t.Hold.Until}
	}
	return out
}

// attachmentDTO is the contract's Attachment. A file removed by a redaction
// is the placeholder {id, removed: true}: every other member is left out,
// the file name included (it can be as sensitive as the file).
type attachmentDTO struct {
	Id          string     `json:"id"`
	Filename    string     `json:"filename,omitempty" doc:"Absent on a removed attachment."`
	Size        int64      `json:"size,omitempty" doc:"Bytes. Absent on a removed attachment."`
	ContentType string     `json:"contentType,omitempty" doc:"Absent on a removed attachment."`
	ScanStatus  string     `json:"scanStatus,omitempty" enum:"unscanned" doc:"Absent on a removed attachment."`
	DownloadURL string     `json:"downloadUrl,omitempty" doc:"Absent on a removed attachment."`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty" doc:"Only on a staged (not yet attached) upload."`
	Removed     bool       `json:"removed,omitempty" doc:"true when a administrator removed the file with a redaction; the placeholder then carries only id. Absent otherwise."`
}

func toAttachment(a tickets.Attachment) attachmentDTO {
	if a.Removed() {
		return attachmentDTO{Id: a.Id, Removed: true}
	}
	out := attachmentDTO{Id: a.Id, Filename: a.Filename, Size: a.Size, ContentType: a.ContentType, ScanStatus: "unscanned",
		DownloadURL: "/api/v1/attachments/" + a.Id + "/download"}
	if a.State == tickets.AttachmentStaged && !a.ExpiresAt.IsZero() {
		exp := a.ExpiresAt
		out.ExpiresAt = &exp
	}
	return out
}

type authorDTO struct {
	Id      string `json:"id" doc:"User, staff token or contact ID. Empty for staff and bot authors when the caller is a requester-side token."`
	Name    string `json:"name"`
	Context string `json:"context" enum:"staff,bot,contact" doc:"staff: a user; bot: a staff token (support staff acting under the token's name); contact: the requester."`
}

// messageDTO is the contract's Message.
type messageDTO struct {
	Id          string          `json:"id"`
	TicketId    string          `json:"ticketId"`
	Visibility  string          `json:"visibility" enum:"public,agent"`
	Author      authorDTO       `json:"author"`
	Body        string          `json:"body"`
	Attachments []attachmentDTO `json:"attachments" doc:"Every file the message was sent with, oldest first; files removed by a redaction are {id, removed: true} placeholders."`
	CreatedAt   time.Time       `json:"createdAt"`
	// RedactedAt is set when an administrator redacted the body (the body
	// is then "[redacted]").
	RedactedAt *time.Time `json:"redactedAt"`
}

func toMessage(m tickets.Message) messageDTO {
	out := messageDTO{Id: m.Id, TicketId: m.TicketId, Visibility: m.Visibility, Author: authorDTO{Id: m.Author.Id, Name: m.Author.Name, Context: m.Author.Context()},
		Body: m.Body, Attachments: make([]attachmentDTO, 0, len(m.Attachments)), CreatedAt: m.CreatedAt}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, toAttachment(a))
	}
	if m.Redacted != nil {
		at := m.Redacted.At
		out.RedactedAt = &at
	}
	return out
}

// postedMessageDTO is a Message plus the ticket after the change.
type postedMessageDTO struct {
	messageDTO
	Ticket ticketDTO `json:"ticket"`
}

// scheduledMessageDTO is a message waiting to be sent (author and project
// administrators only; never integrations).
type scheduledMessageDTO struct {
	Id               string          `json:"id"`
	TicketId         string          `json:"ticketId"`
	Visibility       string          `json:"visibility" enum:"public,agent"`
	Author           authorDTO       `json:"author"`
	Body             string          `json:"body"`
	Attachments      []attachmentDTO `json:"attachments"`
	ThenStatusId     *string         `json:"thenStatusId"`
	AddSignature     bool            `json:"addSignature" doc:"false: the signatures are skipped when sent."`
	SendAt           time.Time       `json:"sendAt"`
	State            string          `json:"state" enum:"scheduled,needsReview,sent,cancelled"`
	Reason           *string         `json:"reason" doc:"Why it is held or cancelled: newerRequesterReply, statusChangeNotAllowed, invalidMessage, ticketArchived, ticketMissing, authorNoAccess, attachmentsUnavailable, cancelledByAuthor, cancelledByAdmin."`
	BaseLastPublicId *string         `json:"baseLastPublicId" doc:"Newest public message when it was scheduled or last reviewed."`
	SentMessageId    *string         `json:"sentMessageId"`
	SentAt           *time.Time      `json:"sentAt"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	Version          int64           `json:"version"`
}

func toScheduled(m tickets.ScheduledMessage) scheduledMessageDTO {
	out := scheduledMessageDTO{Id: m.Id, TicketId: m.TicketId, Visibility: m.Visibility,
		Author: authorDTO{Id: m.AuthorId, Name: m.AuthorName, Context: "staff"}, Body: m.Body,
		Attachments: make([]attachmentDTO, 0, len(m.Attachments)), ThenStatusId: nullable(m.ThenStatusId), AddSignature: !m.NoSignature, SendAt: m.SendAt, State: m.State,
		Reason: nullable(m.Reason), BaseLastPublicId: nullable(m.BaseLastPublicId), SentMessageId: nullable(m.SentMessageId), SentAt: m.SentAt,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Version: m.Version}
	for _, a := range m.Attachments {
		dto := toAttachment(a)
		dto.ExpiresAt = nil // reserved: no expiry while scheduled
		out.Attachments = append(out.Attachments, dto)
	}
	return out
}

type draftDTO struct {
	Visibility       string     `json:"visibility" enum:"public,agent"`
	Body             string     `json:"body"`
	BaseLastPublicId *string    `json:"baseLastPublicId"`
	UpdatedAt        *time.Time `json:"updatedAt"`
	Version          int64      `json:"version"`
}

func toDraft(d tickets.Draft) draftDTO {
	out := draftDTO{Visibility: d.Visibility, Body: d.Body, BaseLastPublicId: nullable(d.BaseLastPublicId), Version: d.Version}
	if !d.UpdatedAt.IsZero() {
		at := d.UpdatedAt
		out.UpdatedAt = &at
	}
	return out
}

type queueCountsDTO struct {
	MyWork           int64 `json:"myWork"`
	Unassigned       int64 `json:"unassigned"`
	Watching         int64 `json:"watching"`
	AwaitingCustomer int64 `json:"awaitingCustomer"`
	OnHold           int64 `json:"onHold"`
	DueSoon          int64 `json:"dueSoon"`
}
