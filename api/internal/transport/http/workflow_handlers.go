package http

import (
	"github.com/arksnorman/open-kase/api/package/fault"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

func registerWorkflow(p *gin.RouterGroup, a *api) {
	p.GET("/tickets/:t/links", a.listLinks)
	p.POST("/tickets/:t/links", a.addLink)
	p.DELETE("/tickets/:t/links/:l", a.removeLink)
	p.POST("/tickets/:t/follow-up", a.idempotent(a.createFollowUp))
	p.POST("/tickets/:t/copy", a.idempotent(a.copyTicket))
	p.GET("/tickets/:t/activity", a.listActivity)
	p.POST("/tickets/:t/messages/:msg/redact", a.idempotent(a.redactMessage))
	if a.Automation != nil {
		p.GET("/automation", a.getAutomation)
		p.PUT("/automation", a.putAutomation)
	}
	if a.Live != nil {
		p.GET("/events", a.streamEvents)
	}
}

// ---- Links ----

// linkedTicketDTO is the other ticket of a link, in the caller's
// projection.
type linkDTO struct {
	Id string `json:"id"`
	// Type is the stored relation, read from source to target.
	Type string `json:"type" enum:"relatedTo,duplicateOf,followUpTo,copiedFrom"`
	// Relation names the link from the ticket in the path.
	Relation  string    `json:"relation" enum:"relatedTo,duplicateOf,duplicatedBy,followUpTo,followedUpBy,copiedFrom,copiedTo"`
	Direction string    `json:"direction" enum:"outgoing,incoming"`
	Ticket    ticketDTO `json:"ticket" doc:"The other ticket (only links whose other ticket the caller may read are returned)."`
	CreatedAt time.Time `json:"createdAt"`
}

func toLink(v tickets.LinkView) linkDTO {
	return linkDTO{Id: v.Link.Id, Type: v.Link.Type, Relation: v.Relation, Direction: v.Direction, Ticket: toTicket(v.Other), CreatedAt: v.Link.CreatedAt}
}

func (a *api) listLinks(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.Tickets.ListLinks(c.Request.Context(), s, c.Param("t"))
	respond(c, http.StatusOK, items(rows, toLink), err)
}

// addLinkRequest is the request body of addLink.
type addLinkRequest struct {
	Type   string `json:"type" required:"true" enum:"relatedTo,duplicateOf,followUpTo" doc:"Relation of the ticket in the path to the other ticket."`
	Ticket string `json:"ticket" required:"true" doc:"The other ticket's key or ID (same project)."`
}

func (a *api) addLink(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body addLinkRequest
	if !decode(c, &body) {
		return
	}
	v, err := a.Tickets.AddLink(c.Request.Context(), s, c.Param("t"), tickets.LinkInput{Type: body.Type, Ticket: body.Ticket})
	respond(c, http.StatusCreated, toLink(v), err)
}

func (a *api) removeLink(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Tickets.RemoveLink(c.Request.Context(), s, c.Param("t"), c.Param("l")))
}

// followUpRequest is the request body of createFollowUp.
type followUpRequest struct {
	Subject       *string  `json:"subject" doc:"Defaults to the original ticket's subject."`
	Body          *string  `json:"body" doc:"First public message; defaults to \"Follow-up to <KEY>.\""`
	Priority      *int     `json:"priority" minimum:"1" maximum:"4"`
	AddSignature  *bool    `json:"addSignature" doc:"Users only: false skips your signature and the workspace signature on the first message."`
	GroupId       *string  `json:"groupId" doc:"Defaults to the original's group (Triage when inactive)."`
	RequestTypeId *string  `json:"requestTypeId" doc:"Defaults to the original's request type."`
	Tags          []string `json:"tags" doc:"Catalog tags for the follow-up (none by default)."`
}

func (a *api) createFollowUp(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body followUpRequest
	if !decode(c, &body) {
		return
	}
	t, err := a.Tickets.FollowUp(c.Request.Context(), s, c.Param("t"), tickets.FollowUpInput{Subject: body.Subject, Body: body.Body, Priority: body.Priority,
		GroupId: body.GroupId, RequestTypeId: body.RequestTypeId, Tags: body.Tags,
		NoSignature: body.AddSignature != nil && !*body.AddSignature})
	if err == nil {
		c.Header("Location", "/api/v1/tickets/"+strconv.FormatInt(t.Number, 10))
	}
	respond(c, http.StatusCreated, toTicket(t), err)
}

// copyRequest is the request body of copyTicket.
type copyRequest struct {
	Subject            *string   `json:"subject" doc:"Defaults to the original's subject."`
	GroupId            *string   `json:"groupId" doc:"Defaults to the original's group (the Triage group when it is inactive)."`
	Priority           *int      `json:"priority" minimum:"1" maximum:"4" doc:"Defaults to the original's priority."`
	RequestTypeId      *string   `json:"requestTypeId" doc:"Defaults to the original's request type."`
	Tags               *[]string `json:"tags" doc:"Catalog tags; defaults to the original's tags that still exist."`
	Body               *string   `json:"body" doc:"First public message; defaults to the original's first public message."`
	IncludeAttachments *bool     `json:"includeAttachments" doc:"Copy the original first message's attachments (default true)."`
}

func (a *api) copyTicket(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body copyRequest
	if !decode(c, &body) {
		return
	}
	t, err := a.Tickets.Copy(c.Request.Context(), s, c.Param("t"), tickets.CopyInput{Subject: body.Subject, GroupId: body.GroupId, Priority: body.Priority,
		RequestTypeId: body.RequestTypeId, Tags: body.Tags, Body: body.Body, IncludeAttachments: body.IncludeAttachments})
	if err == nil {
		c.Header("Location", "/api/v1/tickets/"+strconv.FormatInt(t.Number, 10))
	}
	respond(c, http.StatusCreated, toTicket(t), err)
}

// ---- Activity ----

type activityActorDTO struct {
	Type string  `json:"type" enum:"user,service,operator,system" doc:"service: an API token (integration); its name is the token's name."`
	Id   *string `json:"id"`
	Name string  `json:"name"`
	// Scopes name the API token scopes that allowed the
	// action; empty for users and the system.
	Scopes []string `json:"scopes" doc:"API token scopes that allowed the action (empty for users and the system)."`
}

// activityDTO is one staff-safe timeline entry: field names, identifiers
// and reasons only — never message bodies or field values.
type activityDTO struct {
	Id        string           `json:"id"`
	Action    string           `json:"action" doc:"Audit action, e.g. ticket.claimed, ticket.statusChanged, message.created, ticket.linkAdded, message.redacted."`
	Actor     activityActorDTO `json:"actor"`
	Target    string           `json:"target" doc:"Affected record, e.g. ticket/<id>, message/<id> or ticketLink/<id>."`
	Changes   []string         `json:"changes" doc:"Changed field names (message events: visibility:<audience>)."`
	Reason    *string          `json:"reason"`
	Outcome   string           `json:"outcome" enum:"success,failure"`
	CreatedAt time.Time        `json:"createdAt"`
}

func toActivity(e tickets.ActivityEvent) activityDTO {
	return activityDTO{Id: e.Id, Action: e.Action, Actor: activityActorDTO{Type: e.Actor.Type, Id: nullable(e.Actor.Id), Name: e.Actor.Name, Scopes: nonNil(e.Actor.Scopes)},
		Target: e.Target, Changes: e.Changes, Reason: nullable(e.Reason), Outcome: e.Outcome, CreatedAt: e.CreatedAt}
}

func (a *api) listActivity(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	if o := c.Query("order"); o != "" && o != "asc" && o != "desc" {
		writeError(c, fault.Field("order", "invalidChoice", "Use asc or desc."))
		return
	}
	p, err := a.Tickets.ListActivity(c.Request.Context(), s, c.Param("t"), tickets.ActivityFilter{NewestFirst: c.Query("order") == "desc", Action: c.Query("action")}, after, limit)
	respond(c, http.StatusOK, page(p, toActivity), err)
}

// ---- Redaction ----

// redactMessageRequest is the request body of redactMessage.
type redactMessageRequest struct {
	Reason string `json:"reason" required:"true" doc:"Why the content is removed (3–1000 characters); stored in the audit log."`
	// RemoveAttachments: owner-approved follow-up P-DATA-1.
	RemoveAttachments bool `json:"removeAttachments" doc:"Also remove the message's attachments (default false: they are kept). Removed files are no longer served (download: 410 attachmentRemoved), are listed as {id, removed: true} placeholders and their bytes are deleted shortly after; this cannot be undone. On a message whose body was already redacted, removes the attachments kept then."`
}

func (a *api) redactMessage(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body redactMessageRequest
	if !decode(c, &body) {
		return
	}
	m, err := a.Tickets.RedactMessage(c.Request.Context(), s, c.Param("t"), c.Param("msg"),
		tickets.RedactInput{Reason: body.Reason, RemoveAttachments: body.RemoveAttachments})
	respond(c, http.StatusOK, toMessage(m), err)
}

// ---- Automation policy ----

// automationDTO is the lifecycle automation.
type automationDTO struct {
	ArchiveAfterResolvedDays int `json:"archiveAfterResolvedDays" doc:"Archive tickets resolved this many days ago (0 = disabled)."`
	CustomerFollowupHours    int `json:"customerFollowupAfterHours" doc:"Remind the requester after a ticket waited this long for them (0 = disabled)."`
	MaxReminders             int `json:"maxReminders"`
	ReminderCooldownHours    int `json:"reminderCooldownHours"`
	AutoResolveAfterHours    int `json:"autoResolveAfterHours" doc:"Resolve a waiting ticket this long after the last reminder (0 = disabled; requires reminders)."`
	// Enabled reports whether any rule is active.
	Enabled   bool       `json:"enabled"`
	Version   int64      `json:"version" doc:"0 until first saved; send it as If-Match."`
	UpdatedAt *time.Time `json:"updatedAt"`
}

func toAutomation(p automation.Policy) automationDTO {
	out := automationDTO{ArchiveAfterResolvedDays: p.ArchiveAfterResolvedDays, CustomerFollowupHours: p.FollowupAfterHours,
		MaxReminders: p.MaxReminders, ReminderCooldownHours: p.ReminderCooldownHours, AutoResolveAfterHours: p.AutoResolveAfterHours,
		Enabled: p.Enabled(), Version: p.Version}
	if !p.UpdatedAt.IsZero() {
		at := p.UpdatedAt
		out.UpdatedAt = &at
	}
	return out
}

func (a *api) getAutomation(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	p, err := a.Automation.Get(c.Request.Context(), s)
	respond(c, http.StatusOK, toAutomation(p), err)
}

// putAutomationRequest replaces the automation settings.
type putAutomationRequest struct {
	ArchiveAfterResolvedDays int `json:"archiveAfterResolvedDays" minimum:"0" maximum:"3650"`
	CustomerFollowupHours    int `json:"customerFollowupAfterHours" minimum:"0" maximum:"2160"`
	MaxReminders             int `json:"maxReminders" required:"true" minimum:"1" maximum:"5"`
	ReminderCooldownHours    int `json:"reminderCooldownHours" required:"true" minimum:"1" maximum:"2160"`
	AutoResolveAfterHours    int `json:"autoResolveAfterHours" minimum:"0" maximum:"2160"`
}

func (a *api) putAutomation(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body putAutomationRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatchZero(c)
	if !ok {
		return
	}
	p, err := a.Automation.Put(c.Request.Context(), s, version, automation.Input{ArchiveAfterResolvedDays: body.ArchiveAfterResolvedDays,
		FollowupAfterHours: body.CustomerFollowupHours, MaxReminders: body.MaxReminders, ReminderCooldownHours: body.ReminderCooldownHours,
		AutoResolveAfterHours: body.AutoResolveAfterHours})
	respond(c, http.StatusOK, toAutomation(p), err)
}

// ifMatchZero parses a required If-Match that may be 0 (a resource that
// was never saved).
func ifMatchZero(c *gin.Context) (int64, bool) {
	if raw := c.GetHeader("If-Match"); raw == "0" || raw == `"0"` || raw == `W/"0"` {
		return 0, true
	}
	return ifMatch(c)
}
