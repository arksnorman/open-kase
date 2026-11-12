package http

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/requesttypes"
)

func registerRequestTypes(p *gin.RouterGroup, a *api) {
	p.GET("/request-types", a.listRequestTypes)
	p.POST("/request-types", a.idempotent(a.createRequestType))
	p.PUT("/request-types/order", a.reorderRequestTypes)
	p.PATCH("/request-types/:rt", a.patchRequestType)
	p.DELETE("/request-types/:rt", a.deleteRequestType)
}

type requestTypeDTO struct {
	Id        string    `json:"id"`
	Code      string    `json:"code" doc:"Stable identifier chosen at creation (letters and digits, starting with a lowercase letter); it never changes. Integrations map by code."`
	Name      string    `json:"name"`
	Order     int       `json:"order" doc:"Position in the list; lower comes first."`
	Active    bool      `json:"active" doc:"An inactive type cannot be set on a ticket; tickets that already carry it keep it."`
	InUse     int64     `json:"inUse" doc:"Tickets carrying this type, in any status. Reported to admins; 0 for everyone else."`
	CreatedAt time.Time `json:"createdAt"`
	Version   int64     `json:"version" doc:"Send as If-Match when changing or deleting."`
}

// requestTypeRefDTO is a ticket's request type.
// RequestTypeView returns the contract's RequestType for t (MCP adapter).
func RequestTypeView(t requesttypes.Type) any { return toRequestType(t) }

type requestTypeRefDTO struct {
	Id   string `json:"id"`
	Code string `json:"code" doc:"The type's code, which never changes."`
	Name string `json:"name"`
}

func toRequestType(t requesttypes.Type) requestTypeDTO {
	return requestTypeDTO{Id: t.Id, Code: t.Code, Name: t.Name, Order: t.Order, Active: t.Active, InUse: t.InUse, CreatedAt: t.CreatedAt, Version: t.Version}
}

type createRequestTypeBody struct {
	Name string `json:"name" doc:"1 to 60 characters; unique in the project (case-insensitive), also among inactive types."`
	Code string `json:"code" doc:"Optional. 2 to 40 letters and digits, starting with a lowercase letter; unique in the project. Derived from the name when omitted (\"Service request\" becomes serviceRequest)."`
}

type patchRequestTypeBody struct {
	Name   *string `json:"name"`
	Active *bool   `json:"active" doc:"false deactivates the type, true reactivates it. The last active type cannot be deactivated."`
}

type reorderRequestTypesBody struct {
	Ids []string `json:"ids" doc:"Every request type of the project, active and inactive, once each, in the wanted order."`
}

func (a *api) listRequestTypes(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.RequestTypes.List(c.Request.Context(), s, c.Query("active"))
	respond(c, http.StatusOK, items(rows, toRequestType), err)
}

func (a *api) createRequestType(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body createRequestTypeBody
	if !decode(c, &body) {
		return
	}
	t, err := a.RequestTypes.Create(c.Request.Context(), s, requesttypes.CreateInput{Name: body.Name, Code: body.Code})
	respond(c, http.StatusCreated, toRequestType(t), err)
}

func (a *api) patchRequestType(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body patchRequestTypeBody
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	t, err := a.RequestTypes.Update(c.Request.Context(), s, c.Param("rt"), version, requesttypes.UpdateInput{Name: body.Name, Active: body.Active})
	respond(c, http.StatusOK, toRequestType(t), err)
}

func (a *api) reorderRequestTypes(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body reorderRequestTypesBody
	if !decode(c, &body) {
		return
	}
	rows, err := a.RequestTypes.Reorder(c.Request.Context(), s, body.Ids)
	respond(c, http.StatusOK, items(rows, toRequestType), err)
}

func (a *api) deleteRequestType(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.RequestTypes.Delete(c.Request.Context(), s, c.Param("rt"), version))
}
