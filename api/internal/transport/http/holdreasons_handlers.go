package http

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/holdreasons"
)

func registerHoldReasons(p *gin.RouterGroup, a *api) {
	p.GET("/hold-reasons", a.listHoldReasons)
	p.POST("/hold-reasons", a.idempotent(a.createHoldReason))
	p.PATCH("/hold-reasons/:h", a.patchHoldReason)
	p.DELETE("/hold-reasons/:h", a.deleteHoldReason)
}

type holdReasonDTO struct {
	Id        string    `json:"id"`
	ProjectId string    `json:"projectId"`
	Name      string    `json:"name"`
	MaxDays   int       `json:"maxDays" minimum:"1" maximum:"365" doc:"How long a ticket stays on hold for this reason; the hold ends after exactly this many days."`
	InUse     int64     `json:"inUse" doc:"Tickets currently on hold with this reason. Reported to callers who manage hold reasons (admins, holdReasons:write); 0 for everyone else."`
	CreatedAt time.Time `json:"createdAt"`
	Version   int64     `json:"version"`
}

func toHoldReason(r holdreasons.Reason) holdReasonDTO {
	return holdReasonDTO{Id: r.Id, ProjectId: r.ProjectId, Name: r.Name, MaxDays: r.MaxDays, InUse: r.InUse, CreatedAt: r.CreatedAt, Version: r.Version}
}

// HoldReasonView returns the contract's HoldReason for r (MCP adapter).
func HoldReasonView(r holdreasons.Reason) any { return toHoldReason(r) }

type holdReasonBody struct {
	Name    *string `json:"name" doc:"1 to 80 characters; unique in the project (case-insensitive)."`
	MaxDays *int    `json:"maxDays" minimum:"1" maximum:"365"`
}

func (b holdReasonBody) input() holdreasons.Input {
	return holdreasons.Input{Name: b.Name, MaxDays: b.MaxDays}
}

func (a *api) listHoldReasons(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.HoldReasons.List(c.Request.Context(), s)
	respond(c, http.StatusOK, items(rows, toHoldReason), err)
}

func (a *api) createHoldReason(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body holdReasonBody
	if !decode(c, &body) {
		return
	}
	r, err := a.HoldReasons.Create(c.Request.Context(), s, body.input())
	respond(c, http.StatusCreated, toHoldReason(r), err)
}

func (a *api) patchHoldReason(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body holdReasonBody
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	r, err := a.HoldReasons.Update(c.Request.Context(), s, c.Param("h"), version, body.input())
	respond(c, http.StatusOK, toHoldReason(r), err)
}

func (a *api) deleteHoldReason(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.HoldReasons.Delete(c.Request.Context(), s, c.Param("h"), version))
}
