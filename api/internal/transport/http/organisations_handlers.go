package http

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/organisations"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

func registerOrganisations(p *gin.RouterGroup, a *api) {
	p.GET("/organisations", a.listOrganisations)
	p.POST("/organisations", a.idempotent(a.createOrganisation))
	p.GET("/organisations/:o", a.getOrganisation)
	p.PATCH("/organisations/:o", a.patchOrganisation)
	p.DELETE("/organisations/:o", a.deleteOrganisation)
}

// organisationRefDTO is the organisation embedded in tickets and webhooks.
type organisationRefDTO struct {
	Id          string  `json:"id"`
	Name        string  `json:"name"`
	ExternalRef *string `json:"externalRef" doc:"The integration's own ID for the organisation; null when not set."`
}

func toOrganisationRef(o *tickets.OrgRef) *organisationRefDTO {
	if o == nil {
		return nil
	}
	return &organisationRefDTO{Id: o.Id, Name: o.Name, ExternalRef: nullable(o.ExternalRef)}
}

type organisationDTO struct {
	Id           string    `json:"id"`
	ProjectId    string    `json:"projectId"`
	Name         string    `json:"name"`
	ExternalRef  *string   `json:"externalRef" doc:"The integration's own ID for the organisation (unique in the project); null when not set."`
	Active       bool      `json:"active" doc:"Inactive organisations cannot be linked to new tickets or contacts (422 organisationInactive); existing links keep showing the name."`
	ContactCount int64     `json:"contactCount" doc:"Contacts linked to the organisation. Reported to callers who manage organisations (admins, organisations:write); 0 for everyone else."`
	TicketCount  int64     `json:"ticketCount" doc:"Tickets linked to the organisation (same visibility as contactCount)."`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Version      int64     `json:"version"`
}

func toOrganisation(o organisations.Organisation) organisationDTO {
	return organisationDTO{Id: o.Id, ProjectId: o.ProjectId, Name: o.Name, ExternalRef: nullable(o.ExternalRef), Active: o.Active,
		ContactCount: o.ContactCount, TicketCount: o.TicketCount, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, Version: o.Version}
}

// OrganisationView returns the contract's Organisation for o (MCP adapter).
func OrganisationView(o organisations.Organisation) any { return toOrganisation(o) }

type organisationBody struct {
	Name        *string `json:"name" doc:"1 to 200 characters; unique in the project (case-insensitive). Required on create."`
	ExternalRef *string `json:"externalRef" doc:"Optional, up to 128 characters; unique in the project. Your own ID for the organisation. An empty string clears it."`
	Active      *bool   `json:"active" doc:"PATCH only: false deactivates the organisation, true reactivates it. Omitted on create (new organisations are active)."`
}

func (b organisationBody) input() organisations.Input {
	return organisations.Input{Name: b.Name, ExternalRef: b.ExternalRef, Active: b.Active}
}

func (a *api) listOrganisations(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Organisations.List(c.Request.Context(), s, c.Query("q"), c.Query("active"), c.Query("externalRef"), after, limit)
	respond(c, http.StatusOK, page(p, toOrganisation), err)
}

func (a *api) getOrganisation(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	o, err := a.Organisations.Get(c.Request.Context(), s, c.Param("o"))
	respond(c, http.StatusOK, toOrganisation(o), err)
}

func (a *api) createOrganisation(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body organisationBody
	if !decode(c, &body) {
		return
	}
	o, err := a.Organisations.Create(c.Request.Context(), s, body.input())
	if err == nil {
		c.Header("Location", "/api/v1/organisations/"+o.Id)
	}
	respond(c, http.StatusCreated, toOrganisation(o), err)
}

func (a *api) patchOrganisation(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body organisationBody
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	o, err := a.Organisations.Update(c.Request.Context(), s, c.Param("o"), version, body.input())
	respond(c, http.StatusOK, toOrganisation(o), err)
}

func (a *api) deleteOrganisation(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Organisations.Delete(c.Request.Context(), s, c.Param("o"), version))
}
