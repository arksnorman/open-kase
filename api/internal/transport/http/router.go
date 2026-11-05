// Package http is the Gin REST adapter. Handlers stay thin: they decode,
// call application services and encode. No business rules live here.
package http

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/product"
)

// Deps are the router's collaborators.
type Deps struct {
	Logger    *slog.Logger
	Readiness *Readiness
	// TrustedProxies lists proxy CIDRs/IPs whose forwarding headers are
	// trusted for client IPs. Empty trusts none.
	TrustedProxies []string
	// Services enables the /api/v1 business routes when Auth is set.
	Services Services
	// UIDir, when set, serves the built SPA from this directory.
	UIDir string
	// MCP, when set (and Services.Auth is set), is mounted at /mcp.
	MCP MCPHandler
	// RateLimits overrides DefaultRateLimits per class (PerMinute < 0
	// disables a class).
	RateLimits map[string]RateLimit
}

// NewRouter builds the HTTP handler.
func NewRouter(d Deps) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if err := r.SetTrustedProxies(d.TrustedProxies); err != nil {
		return nil, err
	}
	r.HandleMethodNotAllowed = true
	r.Use(baseMiddleware(d.Logger)...)

	r.GET("/health/live", live)
	r.GET("/health/ready", d.Readiness.handle)

	v1 := r.Group("/api/v1")
	v1.GET("/meta", metaHandler(d.Services.Settings))
	v1.GET("/openapi.json", serveOpenAPI)
	if d.Services.Auth != nil {
		now := time.Now
		if d.Services.Clock != nil {
			now = d.Services.Clock.Now
		}
		a := &api{log: d.Logger, Services: d.Services, limiters: newRateLimiters(d.RateLimits, now),
			liveStreams: newStreamCounter(MaxLiveStreamsPerPrincipal)}
		registerAPI(v1, a)
		if d.MCP != nil {
			mountMCP(r, a, d.MCP)
		}
	}

	var ui *spa
	if d.UIDir != "" {
		var err error
		if ui, err = newSPA(d.UIDir); err != nil {
			return nil, err
		}
	}
	r.NoRoute(func(c *gin.Context) {
		if ui != nil && ui.serve(c) {
			return
		}
		writeError(c, fault.NotFound("routeNotFound"))
	})
	r.NoMethod(func(c *gin.Context) {
		writeError(c, fault.New(fault.KindMethodNotAllowed, "methodNotAllowed", "This method is not allowed for the route."))
	})
	return r, nil
}

func registerAPI(v1 *gin.RouterGroup, a *api) {
	authg := v1.Group("/auth", a.rateLimit)
	authg.POST("/login", a.login)
	authg.POST("/refresh", a.refresh)
	authg.POST("/logout", a.logout)

	p := v1.Group("", a.authenticate, a.rateLimit)
	p.GET("/me", a.getMe)
	p.PATCH("/me", a.patchMe)
	p.PUT("/me/preferences", a.putPreferences)
	p.POST("/me/password", a.changeMyPassword)

	p.GET("/settings", a.getSettings)
	p.PUT("/settings", a.putSettings)

	p.GET("/users", a.listUsers)
	p.POST("/users", a.idempotent(a.createUser))
	p.GET("/users/:u", a.getUser)
	p.PATCH("/users/:u", a.patchUser)
	p.PUT("/users/:u/password", a.setUserPassword)
	p.GET("/users/:u/groups", a.getUserGroups)
	p.PUT("/users/:u/groups", a.putUserGroups)

	p.GET("/integration-scopes", a.listIntegrationScopes)
	registerIdentity(authg, p, a)

	p.GET("/project", a.getProject)
	p.PATCH("/project", a.patchProject)

	p.GET("/groups", a.listGroups)
	p.POST("/groups", a.idempotent(a.createGroup))
	p.GET("/groups/:g", a.getGroup)
	p.PATCH("/groups/:g", a.patchGroup)
	p.DELETE("/groups/:g", a.deleteGroup)
	p.GET("/groups/:g/members", a.listGroupMembers)
	p.PUT("/groups/:g/members/:u", a.putGroupMember)
	p.DELETE("/groups/:g/members/:u", a.deleteGroupMember)

	p.GET("/members", a.listMembers)

	p.GET("/integrations", a.listIntegrations)
	p.POST("/integrations", a.idempotent(a.createIntegration))
	p.PATCH("/integrations/:i", a.patchIntegration)
	p.DELETE("/integrations/:i", a.deleteIntegration)

	p.GET("/contacts", a.listContacts)
	p.POST("/contacts", a.idempotent(a.upsertContact))
	p.GET("/contacts/:c", a.getContact)

	p.GET("/statuses", a.listStatuses)
	p.GET("/request-types", a.listRequestTypes)

	registerTickets(p, a)
	registerWorkflow(p, a)
	registerSearch(p, a)
	if a.Macros != nil && a.SLA != nil && a.Reports != nil {
		registerProductivity(p, a)
	}
	if a.Organisations != nil {
		registerOrganisations(p, a)
	}
	if a.HoldReasons != nil {
		registerHoldReasons(p, a)
	}
	if a.Notifications != nil && a.Webhooks != nil {
		registerNotifications(p, a)
	}
	if a.AuditLog != nil {
		p.GET("/audit-events", a.listAuditEvents)
		p.GET("/audit-actions", a.listAuditActions)
		p.GET("/audit-actors", a.listAuditActors)
	}
}

func live(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "alive"})
}

// metaDTO is the API identity.
type metaDTO struct {
	Product    string `json:"product"`
	APIVersion string `json:"apiVersion"`
}

// metaHandler returns non-sensitive API identity for clients; product is the configured product name. Build versions are
// intentionally omitted from unauthenticated responses.
func metaHandler(svc *settings.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := product.Name
		if svc != nil {
			if snap := svc.Current(); snap != nil && snap.Product().Name != "" {
				name = snap.Product().Name
			}
		}
		c.JSON(http.StatusOK, metaDTO{Product: name, APIVersion: "v1"})
	}
}

// writeError renders err as problem details with the request ID.
func writeError(c *gin.Context, err error) {
	fe := fault.As(err)
	p := fault.ToProblem(err, RequestIDFrom(c))
	if p.Status >= 500 {
		_ = c.Error(err) // surfaced in the access log
	}
	if fe.RetryAfter > 0 {
		c.Header("Retry-After", fmtSeconds(fe.RetryAfter))
	}
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(p.Status, p)
}

func fmtSeconds(d time.Duration) string {
	s := int64((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return strconv.FormatInt(s, 10)
}
