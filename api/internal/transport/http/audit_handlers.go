package http

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/auditlog"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// auditActorDTO is who performed an audited action.
type auditActorDTO struct {
	Type   string   `json:"type" enum:"user,token,system"`
	Id     *string  `json:"id" doc:"User or integration token ID, or the system actor kind (see GET /audit-actors)."`
	Name   string   `json:"name" doc:"User display name (or username), the integration token's name, or the system actor's name such as \"System · Automation\"."`
	Scopes []string `json:"scopes" doc:"Integration token scopes that allowed the action (tokens only; empty otherwise)."`
}

// auditTicketDTO identifies a ticket target.
type auditTicketDTO struct {
	Id      string `json:"id"`
	Number  int64  `json:"number" doc:"Shown as #1042."`
	Subject string `json:"subject"`
}

// auditTargetDTO is what an audited action was applied to.
type auditTargetDTO struct {
	Type   string          `json:"type" doc:"Entity type, e.g. ticket, user, contact, group, integration, webhook, settings."`
	Id     *string         `json:"id"`
	Name   *string         `json:"name" doc:"Display name when known (ticket subject, user or contact name, webhook URL, …)."`
	Ticket *auditTicketDTO `json:"ticket" doc:"Set when the event belongs to a ticket that still exists."`
}

// auditDetailsDTO is the redacted detail of an event: changed field names
// (never values), the recorded reason and the actor's stored label.
type auditDetailsDTO struct {
	Changes []string `json:"changes"`
	Reason  *string  `json:"reason"`
	Outcome string   `json:"outcome" enum:"success,failure"`
	Label   *string  `json:"label" doc:"Non-secret description of a system or CLI actor."`
}

// auditEventDTO is one audit log entry.
type auditEventDTO struct {
	Id          string          `json:"id"`
	CreatedAt   time.Time       `json:"createdAt"`
	Action      string          `json:"action"`
	ActionLabel string          `json:"actionLabel" doc:"Human label from the action catalog (GET /audit-actions); the code itself when it is not catalogued."`
	Actor       auditActorDTO   `json:"actor"`
	Target      *auditTargetDTO `json:"target"`
	RequestId   *string         `json:"requestId"`
	Details     auditDetailsDTO `json:"details"`
}

func toAuditEvent(e auditlog.Entry) auditEventDTO {
	scopes := e.Actor.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	changes := e.Changes
	if changes == nil {
		changes = []string{}
	}
	out := auditEventDTO{Id: e.Id, CreatedAt: e.CreatedAt, Action: e.Action, ActionLabel: audit.ActionLabel(e.Action), RequestId: nullable(e.RequestId),
		Actor:   auditActorDTO{Type: e.Actor.Type, Id: nullable(e.Actor.Id), Name: e.Actor.Name, Scopes: scopes},
		Details: auditDetailsDTO{Changes: changes, Reason: nullable(e.Reason), Outcome: e.Outcome, Label: nullable(e.Actor.Label)}}
	if t := e.Target; t != nil {
		out.Target = &auditTargetDTO{Type: t.Type, Id: nullable(t.Id), Name: nullable(t.Name)}
		if t.Ticket != nil {
			out.Target.Ticket = &auditTicketDTO{Id: t.Ticket.Id, Number: t.Ticket.Number, Subject: t.Ticket.Subject}
		}
	}
	return out
}

func (a *api) listAuditEvents(c *gin.Context) {
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.AuditLog.List(c.Request.Context(), principal(c), auditlog.Query{ActorType: c.Query("actorType"), ActorId: c.Query("actorId"),
		Action: c.Query("action"), Ticket: c.Query("ticket"), From: c.Query("from"), To: c.Query("to"), After: after, Limit: limit})
	respond(c, http.StatusOK, page(p, toAuditEvent), err)
}

// auditActionDTO is one entry of the action catalog.
type auditActionDTO struct {
	Code     string `json:"code"`
	Label    string `json:"label" doc:"Human label, e.g. \"Created a ticket\"."`
	Category string `json:"category" doc:"Display group, e.g. Tickets, Messages, Users & access, Sign-in & security."`
}

func (a *api) listAuditActions(c *gin.Context) {
	list, err := a.AuditLog.Actions(principal(c))
	if err != nil {
		writeError(c, err)
		return
	}
	out := make([]auditActionDTO, 0, len(list))
	for _, x := range list {
		out = append(out, auditActionDTO{Code: string(x.Code), Label: x.Label, Category: x.Category})
	}
	c.JSON(http.StatusOK, itemsOf(out))
}

// auditActorOptionDTO is one entry of the actor catalog.
type auditActorOptionDTO struct {
	Type   string `json:"type" enum:"user,token,system"`
	Id     string `json:"id" doc:"User or integration token ID, or the system actor kind (automation, scheduler, sla, jobs, signingKeys, installation, cli). Pass type and id as actorType and actorId to GET /audit-events."`
	Name   string `json:"name" doc:"User name (or username), token name, or e.g. \"System · Automation\"."`
	Detail string `json:"detail" doc:"Username and email (and deactivated/invited), token state, or what the system actor does."`
}

// auditActorListDTO is the actor catalog response.
type auditActorListDTO struct {
	Items     []auditActorOptionDTO `json:"items"`
	Truncated bool                  `json:"truncated" doc:"More users or tokens match than the limit; narrow the search."`
}

func (a *api) listAuditActors(c *gin.Context) {
	var limit *int
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			writeError(c, fault.Field("limit", validation.CodeInvalidType, "Use a whole number."))
			return
		}
		limit = &n
	}
	list, truncated, err := a.AuditLog.Actors(c.Request.Context(), principal(c), auditlog.ActorQuery{Q: c.Query("q"), Limit: limit})
	if err != nil {
		writeError(c, err)
		return
	}
	out := auditActorListDTO{Items: make([]auditActorOptionDTO, 0, len(list)), Truncated: truncated}
	for _, x := range list {
		out.Items = append(out.Items, auditActorOptionDTO{Type: x.Type, Id: x.Id, Name: x.Name, Detail: x.Detail})
	}
	c.JSON(http.StatusOK, out)
}
