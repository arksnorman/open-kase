package http

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/access"
)

// MCPHandler is the MCP endpoint (internal/transport/mcp). The REST adapter
// mounts it at /mcp behind the same bearer authentication as /api/v1.
type MCPHandler interface {
	http.Handler
	// CheckOrigin rejects browser requests from origins that are not allowed.
	CheckOrigin(r *http.Request) error
	// Admit decides whether an authenticated principal may use the endpoint
	// now (MCP scope, rate limit).
	Admit(ctx context.Context, p access.Principal) error
}

// mountMCP registers /mcp: Origin validation, then the shared principal middleware
// (integration token or user access token, checked against MongoDB on every
// request), then admission, then the SDK handler.
func mountMCP(r *gin.Engine, a *api, h MCPHandler) {
	origin := func(c *gin.Context) {
		if err := h.CheckOrigin(c.Request); err != nil {
			writeError(c, err)
			return
		}
		c.Next()
	}
	admit := func(c *gin.Context) {
		p := principal(c)
		if err := h.Admit(c.Request.Context(), p); err != nil {
			writeError(c, err)
			return
		}
		c.Request = c.Request.WithContext(access.WithPrincipal(c.Request.Context(), p))
		c.Next()
	}
	r.Match([]string{http.MethodPost, http.MethodGet, http.MethodDelete}, "/mcp", origin, a.authenticate, admit, gin.WrapH(h))
}
