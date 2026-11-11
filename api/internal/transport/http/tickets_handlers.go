package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/fault"
)

func registerTickets(p *gin.RouterGroup, a *api) {
	p.GET("/queue-counts", a.queueCounts)
	p.GET("/tickets", a.listTickets)
	p.POST("/tickets", a.idempotent(a.createTicket))
	p.GET("/tickets/:t", a.getTicket)
	p.PATCH("/tickets/:t", a.patchTicket)
	p.POST("/tickets/:t/claim", a.claimTicket)
	p.POST("/tickets/:t/release", a.releaseTicket)
	p.POST("/tickets/:t/assignments", a.assignTicket)
	p.POST("/tickets/:t/transitions", a.transitionTicket)
	p.GET("/tickets/:t/messages", a.listMessages)
	p.POST("/tickets/:t/messages", a.idempotent(a.postMessage))
	p.GET("/tickets/:t/scheduled-messages", a.listScheduled)
	p.POST("/tickets/:t/scheduled-messages", a.scheduleMessage)
	p.PATCH("/tickets/:t/scheduled-messages/:s", a.updateScheduled)
	p.POST("/tickets/:t/scheduled-messages/:s/cancel", a.cancelScheduled)
	p.POST("/tickets/:t/scheduled-messages/:s/send", a.sendScheduledNow)
	p.GET("/tickets/:t/watchers", a.listWatchers)
	p.PUT("/tickets/:t/watchers/me", a.watchTicket)
	p.DELETE("/tickets/:t/watchers/me", a.unwatchTicket)
	p.GET("/tickets/:t/drafts/:visibility", a.getDraft)
	p.PUT("/tickets/:t/drafts/:visibility", a.putDraft)
	p.DELETE("/tickets/:t/drafts/:visibility", a.deleteDraft)
	p.POST("/attachments", a.uploadAttachment)
	p.GET("/attachments/:a/download", a.downloadAttachment)
}

// optionalIfMatch parses If-Match when present; 0 means "not sent".
func optionalIfMatch(c *gin.Context) (int64, bool) {
	if strings.TrimSpace(c.GetHeader("If-Match")) == "" {
		return 0, true
	}
	return ifMatch(c)
}

func (a *api) queueCounts(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	q, err := a.Tickets.QueueCounts(c.Request.Context(), s)
	respond(c, http.StatusOK, queueCountsDTO{MyWork: q.MyWork, Unassigned: q.Unassigned, Watching: q.Watching, AwaitingCustomer: q.AwaitingCustomer, OnHold: q.OnHold, DueSoon: q.DueSoon}, err)
}

func (a *api) listTickets(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Tickets.List(c.Request.Context(), s, tickets.ListInput{View: c.Query("view"), Q: c.Query("q"),
		GroupId: c.Query("groupId"), After: after, Limit: limit, Filter: listFilter(c)})
	respond(c, http.StatusOK, page(p, toTicket), err)
}

type requesterBody struct {
	ContactId      *string `json:"contactId"`
	Name           *string `json:"name"`
	Email          *string `json:"email"`
	ExternalId     *string `json:"externalId"`
	OrganisationId *string `json:"organisationId" doc:"Optional with name and email: link a new or updated requester contact to this organisation. Explicit only; never inferred."`
}

// createTicketRequest is the request body of createTicket.
type createTicketRequest struct {
	Subject        string        `json:"subject" required:"true"`
	Body           string        `json:"body" required:"true"`
	Requester      requesterBody `json:"requester" required:"true" doc:"An existing contactId, or name and email (plus optional externalId) to find or create the contact."`
	Priority       *int          `json:"priority" minimum:"1" maximum:"4"`
	RequestTypeId  *string       `json:"requestTypeId"`
	GroupId        *string       `json:"groupId"`
	AttachmentIds  []string      `json:"attachmentIds"`
	OrganisationId *string       `json:"organisationId" doc:"Optional: the organisation (from POST /organisations) this ticket belongs to. Set explicitly; it is not taken from the requester."`
	CCEmails       []string      `json:"ccEmails" doc:"Optional: up to 10 addresses copied on every email to the requester (not the requester's own address)."`
}

func (a *api) createTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body createTicketRequest
	if !decode(c, &body) {
		return
	}
	t, err := a.Tickets.Create(c.Request.Context(), s, tickets.CreateInput{
		Subject: body.Subject, Body: body.Body, Priority: body.Priority, RequestTypeId: body.RequestTypeId, GroupId: body.GroupId,
		AttachmentIds: body.AttachmentIds, OrganisationId: body.OrganisationId, CCEmails: body.CCEmails,
		Requester: tickets.RequesterInput{ContactId: body.Requester.ContactId, Name: body.Requester.Name,
			Email: body.Requester.Email, ExternalId: body.Requester.ExternalId, OrganisationId: body.Requester.OrganisationId},
	})
	if err == nil {
		c.Header("Location", "/api/v1/tickets/"+strconv.FormatInt(t.Number, 10))
	}
	respond(c, http.StatusCreated, toTicket(t), err)
}

func (a *api) getTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	t, err := a.Tickets.Get(c.Request.Context(), s, c.Param("t"))
	respond(c, http.StatusOK, toTicket(t), err)
}

// patchTicketRequest is the request body of patchTicket.
type patchTicketRequest struct {
	Subject        *string   `json:"subject"`
	Priority       *int      `json:"priority" minimum:"1" maximum:"4"`
	RequestTypeId  *string   `json:"requestTypeId"`
	Tags           *[]string `json:"tags"`
	OrganisationId *string   `json:"organisationId" doc:"Staff only. Links the ticket to an organisation; an empty string clears it. Signed-in users get 403 organisationManagedByIntegration when the ticket was created through the API or MCP; only integration credentials may change it."`
	CCEmails       *[]string `json:"ccEmails" doc:"Staff only. Replaces the CC addresses (up to 10; lower-cased and deduplicated; not the requester's address); an empty list clears them."`
}

func (a *api) patchTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body patchTicketRequest
	if !decode(c, &body) {
		return
	}
	// Requester tokens see version 0 and may omit If-Match (subject only;
	// the change applies to the current version).
	var version int64
	if !s.Principal.IsStaff() {
		if strings.TrimSpace(c.GetHeader("If-Match")) != "" {
			version, ok = ifMatchZero(c)
		}
	} else {
		version, ok = ifMatch(c)
	}
	if !ok {
		return
	}
	t, err := a.Tickets.Update(c.Request.Context(), s, c.Param("t"), version,
		tickets.UpdateInput{Subject: body.Subject, Priority: body.Priority, RequestTypeId: body.RequestTypeId,
			Tags: body.Tags, OrganisationId: body.OrganisationId, CCEmails: body.CCEmails})
	respond(c, http.StatusOK, toTicket(t), err)
}

func (a *api) claimTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	t, err := a.Tickets.Claim(c.Request.Context(), s, c.Param("t"), version)
	respond(c, http.StatusOK, toTicket(t), err)
}

// releaseTicketRequest is the request body of releaseTicket.
type releaseTicketRequest struct {
	Reason string `json:"reason" required:"true" doc:"Why the ticket goes back to the group queue (3–500 characters; rule ticket.releaseReason). Recorded in the audit log and shown in the activity timeline."`
}

func (a *api) releaseTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	// Refuse principals that may never release before reading the body.
	if err := s.Principal.RequireUser(); err != nil {
		writeError(c, err)
		return
	}
	var body releaseTicketRequest
	if !decodeOrEmpty(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	t, err := a.Tickets.Release(c.Request.Context(), s, c.Param("t"), version, body.Reason)
	respond(c, http.StatusOK, toTicket(t), err)
}

// optionalID distinguishes an absent JSON member from null.
type optionalID struct {
	set   bool
	value *string
}

func (o *optionalID) UnmarshalJSON(b []byte) error {
	o.set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	o.value = &s
	return nil
}

// assignTicketRequest is the request body of assignTicket.
type assignTicketRequest struct {
	GroupId    string     `json:"groupId" required:"true"`
	AssigneeId optionalID `json:"assigneeId" doc:"Absent: keep an eligible assignee (cleared when ineligible); null: unassign; an ID: assign to that group member."`
}

func (a *api) assignTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body assignTicketRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	t, err := a.Tickets.Assign(c.Request.Context(), s, c.Param("t"), version, tickets.AssignInput{GroupId: body.GroupId,
		AssigneeId: tickets.OptionalID{Set: body.AssigneeId.set, Value: body.AssigneeId.value}})
	respond(c, http.StatusOK, toTicket(t), err)
}

// transitionTicketRequest is the request body of transitionTicket.
type transitionTicketRequest struct {
	Action       string  `json:"action" required:"true" enum:"resolve,reopen,archive,hold,setStatus"`
	StatusId     *string `json:"statusId" doc:"Only with setStatus: an open or awaiting-customer status (On hold is entered with hold)."`
	Reason       string  `json:"reason"`
	HoldReasonId string  `json:"holdReasonId" doc:"Required with hold (and refused otherwise): the hold reason from GET /hold-reasons. The ticket returns to Open after the reason's maxDays."`
	Note         string  `json:"note" doc:"Only with hold: an optional internal note (agent audience) saved with the hold. Needs the internal-note permission (tickets:internal for tokens)."`
	ContactId    string  `json:"contactId" doc:"Only with resolve by a requester-side token: the contact of the ticket's organisation who resolved it (contactNotInOrganisation otherwise; the ticket needs an organisation). Recorded in the audit history. Omitted or the requester's own ID: the requester. Any other caller: 422 notAllowed."`
}

func (a *api) transitionTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body transitionTicketRequest
	if !decode(c, &body) {
		return
	}
	// Requester-side tokens (resolve only) see no version: If-Match is
	// not required and ignored for them.
	var version int64
	if s.Principal.IsStaff() {
		if version, ok = ifMatch(c); !ok {
			return
		}
	}
	t, err := a.Tickets.Transition(c.Request.Context(), s, c.Param("t"), version,
		tickets.TransitionInput{Action: body.Action, StatusId: body.StatusId, Reason: body.Reason, HoldReasonId: body.HoldReasonId, Note: body.Note, ContactId: body.ContactId})
	respond(c, http.StatusOK, toTicket(t), err)
}

// watcherDTO is a staff user watching a ticket.
type watcherDTO struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
}

func (a *api) listWatchers(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.Tickets.ListWatchers(c.Request.Context(), s, c.Param("t"))
	out := make([]watcherDTO, 0, len(rows))
	for _, w := range rows {
		out = append(out, watcherDTO{Id: w.Id, Name: w.Name, Username: w.Username})
	}
	respond(c, http.StatusOK, itemsOf(out), err)
}

func (a *api) watchTicket(c *gin.Context)   { a.setWatch(c, true) }
func (a *api) unwatchTicket(c *gin.Context) { a.setWatch(c, false) }

func (a *api) setWatch(c *gin.Context, watch bool) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	t, err := a.Tickets.Watch(c.Request.Context(), s, c.Param("t"), watch)
	respond(c, http.StatusOK, toTicket(t), err)
}

// ---- Messages ----

func (a *api) listMessages(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Tickets.ListMessages(c.Request.Context(), s, c.Param("t"), c.Query("visibility"), after, limit)
	respond(c, http.StatusOK, page(p, toMessage), err)
}

// postMessageRequest is the request body of postMessage.
type postMessageRequest struct {
	Visibility           string   `json:"visibility" required:"true" enum:"public,agent" doc:"The audience. Required; never inferred."`
	Body                 string   `json:"body" required:"true" doc:"Markdown source; rendered without raw HTML."`
	AttachmentIds        []string `json:"attachmentIds"`
	ExpectedLastPublicId *string  `json:"expectedLastPublicId"`
	ThenStatusId         *string  `json:"thenStatusId" doc:"(added) Also change the ticket status in the same transaction: the built-in Resolved status resolves the ticket, an open or awaiting-customer status is set like setStatus. Users with ticket.transition, tokens with tickets:write (posting as staff). An invalid transition fails the whole request; nothing is posted."`
	AddSignature         *bool    `json:"addSignature" doc:"Users only, public replies: false skips your signature and the workspace signature. Absent or true: the server appends them (never shown in drafts). Never applied to agent notes, integration tokens or requester messages."`
	ContactId            string   `json:"contactId" doc:"Only with as=requester: author the reply as this contact instead of the requester. The contact must belong to the ticket's organisation (contactNotInOrganisation otherwise; the ticket needs an organisation). The reply behaves like any requester reply. Omitted or the requester's own ID: the requester."`
	As                   string   `json:"as" enum:"staff,requester" doc:"Integration tokens only: who authors the message. staff: the token as a bot under its name (public replies need tickets:comment, agent notes tickets:internal). requester: the ticket's requester contact (tickets:replyAsRequester; public only, no thenStatusId). Omitted: staff when the token holds tickets:comment (or tickets:all), otherwise requester. Users always post as themselves (requester: 422)."`
}

func (a *api) postMessage(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body postMessageRequest
	if !decode(c, &body) {
		return
	}
	res, err := a.Tickets.PostMessage(c.Request.Context(), s, c.Param("t"), tickets.PostInput{Visibility: body.Visibility,
		Body: body.Body, AttachmentIds: body.AttachmentIds, ExpectedLastPublicId: body.ExpectedLastPublicId, ThenStatusId: body.ThenStatusId,
		As: body.As, ContactId: body.ContactId, NoSignature: body.AddSignature != nil && !*body.AddSignature})
	respond(c, http.StatusCreated, postedMessageDTO{messageDTO: toMessage(res.Message), Ticket: toTicket(res.Ticket)}, err)
}

// ---- Scheduled messages ----

func parseSendAt(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, fault.Field("sendAt", "invalidFormat", "Send an RFC 3339 date-time with a time zone, e.g. 2026-10-05T08:00:00Z.")
	}
	at = at.UTC().Truncate(time.Second)
	return &at, nil
}

func (a *api) listScheduled(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	state := c.Query("state")
	if state != "" && state != "active" && state != "all" {
		writeError(c, fault.Field("state", "invalidChoice", "Choose active or all."))
		return
	}
	rows, err := a.Tickets.ListScheduled(c.Request.Context(), s, c.Param("t"), state == "all")
	out := make([]scheduledMessageDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, toScheduled(m))
	}
	respond(c, http.StatusOK, itemsOf(out), err)
}

// scheduleMessageRequest is the request body of scheduleMessage.
type scheduleMessageRequest struct {
	Visibility           string   `json:"visibility" required:"true" enum:"public,agent" doc:"The audience. Required; never inferred."`
	Body                 string   `json:"body" required:"true" doc:"Markdown source; rendered without raw HTML."`
	SendAt               *string  `json:"sendAt" required:"true" doc:"RFC 3339 date-time with a zone; stored in UTC. Must be in the future and at most 365 days ahead."`
	AttachmentIds        []string `json:"attachmentIds" doc:"Staged uploads of the caller; they are reserved (no expiry) until the message is sent or cancelled."`
	ExpectedLastPublicId *string  `json:"expectedLastPublicId"`
	ThenStatusId         *string  `json:"thenStatusId" doc:"Status to set when the message is sent (as on postMessage); checked again at send time."`
	AddSignature         *bool    `json:"addSignature" doc:"false skips the signatures when the message is sent; they are appended at send time using the author's signature then."`
}

func (a *api) scheduleMessage(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body scheduleMessageRequest
	if !decode(c, &body) {
		return
	}
	at, err := parseSendAt(body.SendAt)
	if err != nil {
		writeError(c, err)
		return
	}
	in := tickets.ScheduleInput{PostInput: tickets.PostInput{Visibility: body.Visibility, Body: body.Body, AttachmentIds: body.AttachmentIds,
		ExpectedLastPublicId: body.ExpectedLastPublicId, ThenStatusId: body.ThenStatusId,
		NoSignature: body.AddSignature != nil && !*body.AddSignature}}
	if at != nil {
		in.SendAt = *at
	}
	m, err := a.Tickets.Schedule(c.Request.Context(), s, c.Param("t"), in)
	respond(c, http.StatusCreated, toScheduled(m), err)
}

// updateScheduledRequest is the request body of updateScheduled.
type updateScheduledRequest struct {
	Body         *string    `json:"body"`
	SendAt       *string    `json:"sendAt" doc:"RFC 3339 date-time with a zone; must be in the future."`
	AddSignature *bool      `json:"addSignature" doc:"Absent: keep; false: skip the signatures when sent; true: add them."`
	ThenStatusId optionalID `json:"thenStatusId" doc:"Absent: keep; null: no status change; an ID: set that status when sending."`
}

func (a *api) updateScheduled(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body updateScheduledRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	at, err := parseSendAt(body.SendAt)
	if err != nil {
		writeError(c, err)
		return
	}
	m, err := a.Tickets.UpdateScheduled(c.Request.Context(), s, c.Param("t"), c.Param("s"), version, tickets.ScheduleUpdate{
		Body: body.Body, SendAt: at, AddSignature: body.AddSignature, ThenStatusId: tickets.OptionalID{Set: body.ThenStatusId.set, Value: body.ThenStatusId.value}})
	respond(c, http.StatusOK, toScheduled(m), err)
}

func (a *api) cancelScheduled(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	version, ok := optionalIfMatch(c)
	if !ok {
		return
	}
	m, err := a.Tickets.CancelScheduled(c.Request.Context(), s, c.Param("t"), c.Param("s"), version)
	respond(c, http.StatusOK, toScheduled(m), err)
}

// sendScheduledRequest is the (optional) request body of sendScheduledNow.
type sendScheduledRequest struct {
	ExpectedLastPublicId *string `json:"expectedLastPublicId"`
}

func (a *api) sendScheduledNow(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body sendScheduledRequest
	if c.Request.ContentLength != 0 && !decode(c, &body) {
		return
	}
	version, ok := optionalIfMatch(c)
	if !ok {
		return
	}
	res, err := a.Tickets.SendScheduledNow(c.Request.Context(), s, c.Param("t"), c.Param("s"), version, body.ExpectedLastPublicId)
	respond(c, http.StatusCreated, postedMessageDTO{messageDTO: toMessage(res.Message), Ticket: toTicket(res.Ticket)}, err)
}

// ---- Drafts ----

func (a *api) getDraft(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	d, err := a.Tickets.GetDraft(c.Request.Context(), s, c.Param("t"), c.Param("visibility"))
	respond(c, http.StatusOK, toDraft(d), err)
}

// putDraftRequest is the request body of putDraft.
type putDraftRequest struct {
	Body             string  `json:"body"`
	BaseLastPublicId *string `json:"baseLastPublicId"`
}

func (a *api) putDraft(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body putDraftRequest
	if !decode(c, &body) {
		return
	}
	version, ok := optionalIfMatch(c)
	if !ok {
		return
	}
	d, err := a.Tickets.PutDraft(c.Request.Context(), s, c.Param("t"), c.Param("visibility"), body.Body, body.BaseLastPublicId, version)
	respond(c, http.StatusOK, toDraft(d), err)
}

func (a *api) deleteDraft(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Tickets.DeleteDraft(c.Request.Context(), s, c.Param("t"), c.Param("visibility")))
}

// ---- Attachments ----

// multipartOverhead allows for multipart headers around the file part.
const multipartOverhead = 1 << 20

// uploadAttachment streams the multipart field "file" into a staged upload
// without buffering it on local disk.
func (a *api) uploadAttachment(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	extendTransferDeadlines(c, true)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, a.Tickets.MaxUploadBytes()+multipartOverhead)
	mr, err := c.Request.MultipartReader()
	if err != nil {
		writeError(c, fault.New(fault.KindUnsupportedMedia, "multipartRequired", "Upload the file as multipart/form-data in the field \"file\"."))
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			writeError(c, fault.Field("file", "required", "Send the file in the multipart field \"file\"."))
			return
		}
		if err != nil {
			writeError(c, uploadError(err))
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		att, err := a.Tickets.Upload(c.Request.Context(), s, part.FileName(), part)
		_ = part.Close()
		if err != nil {
			writeError(c, uploadError(err))
			return
		}
		c.JSON(http.StatusCreated, toAttachment(att))
		return
	}
}

func uploadError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return fault.New(fault.KindTooLarge, "attachmentTooLarge", "The file is larger than the configured limit.")
	}
	var fe *fault.Error
	if errors.As(err, &fe) {
		return fe
	}
	return fault.Wrap(fault.KindMalformed, "uploadFailed", err)
}

// inlineSafeTypes are content types passed through on download. Anything
// else (HTML, SVG, scripts, unknown) is served as application/octet-stream.
var inlineSafeTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "application/pdf": true,
	"text/plain; charset=utf-8": true, "application/zip": true,
}

func (a *api) downloadAttachment(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	att, rc, err := a.Tickets.Download(c.Request.Context(), s, c.Param("a"))
	if err != nil {
		writeError(c, err)
		return
	}
	defer rc.Close()
	ct := "application/octet-stream"
	if inlineSafeTypes[att.ContentType] {
		ct = att.ContentType
	}
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": att.Filename})
	if disp == "" {
		disp = "attachment"
	}
	h := c.Writer.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", disp)
	h.Set("Content-Length", strconv.FormatInt(att.Size, 10))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	extendTransferDeadlines(c, false)
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, rc); err != nil {
		a.log.Warn("attachment download interrupted", "error", err.Error())
	}
}
