package http

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/auditlog"
	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	livestream "github.com/arksnorman/open-kase/api/internal/live"
	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/organisations"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/reports"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/internal/webhooks/endpoints"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/product"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Services are the application services exposed over REST. Handlers only
// decode input, call a service and encode the result.
type Services struct {
	Auth         *auth.Service
	Integrations *integrations.Service
	Access       *access.Service
	Users        *users.Service
	Projects     *projects.Service
	Customers    *customers.Service
	Settings     *settings.Service
	Workflow     *statuses.Catalog
	Tickets      *tickets.Service
	// Notifications and Webhooks are the Milestone 5 services.
	Notifications *notifications.Service
	Webhooks      *endpoints.Service
	// Milestone 6 services.
	SLA    *sla.Service
	Macros *macros.Service
	// HoldReasons manages the reasons a ticket can be put on hold for.
	HoldReasons *holdreasons.Service
	// Organisations manages the customer organisations an integration serves.
	Organisations *organisations.Service
	Reports       *reports.Service
	// AuditLog lists audit events for the admin Logs area (optional).
	AuditLog *auditlog.Service
	// Ticket workflow: lifecycle automation and live updates (optional).
	Automation *automation.Service
	Live       *livestream.Hub
	// LiveHeartbeat is the SSE heartbeat/re-check interval (0 = default).
	LiveHeartbeat time.Duration
	Idempotency   idempotency.Store
	Clock         clock.Clock
	// SecureCookies sets the Secure attribute on session cookies. It is
	// false only when APP_ENV=development.
	SecureCookies bool
	// BaseURL returns the configured public base URL (may be empty); its
	// origin is accepted by the Origin check in addition to the request Host.
	BaseURL func() string
}

type api struct {
	log *slog.Logger
	Services
	limiters    rateLimiters
	liveStreams *streamCounter
}

const ctxPrincipal = "principal"

// principal returns the authenticated principal set by authenticate.
func principal(c *gin.Context) access.Principal {
	p, _ := c.Get(ctxPrincipal)
	pr, _ := p.(access.Principal)
	return pr
}

// authenticate resolves the bearer credential (user access token or
// integration token) into a principal, or aborts with 401.
func (a *api) authenticate(c *gin.Context) {
	hdr := c.GetHeader("Authorization")
	scheme, raw, ok := strings.Cut(hdr, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(raw) == "" {
		c.Header("WWW-Authenticate", `Bearer realm="`+product.Slug+`"`)
		writeError(c, fault.Unauthenticated("authenticationRequired", "Sign in or provide an API token."))
		return
	}
	raw = strings.TrimSpace(raw)
	var (
		p   access.Principal
		err error
	)
	if integrations.LooksLikeToken(raw) {
		p, err = a.Integrations.Authenticate(c.Request.Context(), raw)
	} else {
		p, err = a.Auth.Authenticate(c.Request.Context(), raw)
	}
	if err != nil {
		if fault.As(err).Kind == fault.KindUnauthenticated {
			c.Header("WWW-Authenticate", `Bearer realm="`+product.Slug+`", error="invalidToken"`)
		}
		writeError(c, err)
		return
	}
	if p.MFAEnrolmentRequired && !mfaEnrolmentRoutes[c.FullPath()] {
		writeError(c, errMFAEnrolmentRequired())
		return
	}
	c.Set(ctxPrincipal, p)
	c.Next()
}

// scope authorizes the principal for the deployment's project.
func (a *api) scope(c *gin.Context) (*access.ProjectScope, bool) {
	s, err := a.Access.Scope(c.Request.Context(), principal(c))
	if err != nil {
		writeError(c, err)
		return nil, false
	}
	return s, true
}

// transferTimeout bounds one attachment upload or download. The server's
// read/write timeouts (60 s) cover a whole request, which a large file on a
// slow connection exceeds (the bundled nginx streams with proxy_buffering
// off); these handlers extend them after authorization.
const transferTimeout = 15 * time.Minute

// extendTransferDeadlines lifts the server's timeouts for this request:
// the write deadline always, the read deadline for uploads.
func extendTransferDeadlines(c *gin.Context, reading bool) {
	rc := http.NewResponseController(c.Writer)
	deadline := time.Now().Add(transferTimeout)
	if reading {
		_ = rc.SetReadDeadline(deadline)
	}
	_ = rc.SetWriteDeadline(deadline)
}

// decode strictly decodes the JSON body into dst.
func decode(c *gin.Context, dst any) bool {
	if err := validation.DecodeStrict(c.Request.Body, validation.DefaultMaxBodyBytes, dst); err != nil {
		writeError(c, err)
		return false
	}
	return true
}

// decodeOrEmpty is decode for commands whose body used to be optional: an
// empty body decodes as {} so that required fields report 422 field errors
// instead of a malformed-JSON 400.
func decodeOrEmpty(c *gin.Context, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, validation.DefaultMaxBodyBytes+1))
	if err != nil {
		writeError(c, fault.Wrap(fault.KindMalformed, "unreadableBody", err))
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	if err := validation.DecodeStrict(bytes.NewReader(raw), validation.DefaultMaxBodyBytes, dst); err != nil {
		writeError(c, err)
		return false
	}
	return true
}

// ifMatch parses the required If-Match version header. It accepts a bare
// integer or a (weak) entity tag such as "3" or W/"3".
func ifMatch(c *gin.Context) (int64, bool) {
	raw := strings.TrimSpace(c.GetHeader("If-Match"))
	if raw == "" {
		writeError(c, fault.New(fault.KindPrecondition, "preconditionRequired", "Send If-Match with the version you are changing."))
		return 0, false
	}
	raw = strings.Trim(strings.TrimPrefix(raw, "W/"), `"`)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 1 {
		writeError(c, fault.New(fault.KindMalformed, "invalidIfMatch", "If-Match must contain a version number."))
		return 0, false
	}
	return v, true
}

// pageParams parses ?cursor=&limit=.
func pageParams(c *gin.Context) (*pagination.Cursor, int, bool) {
	limit, err := pagination.ParseLimit(c.Query("limit"))
	if err != nil {
		writeError(c, err)
		return nil, 0, false
	}
	cur, err := pagination.Decode(c.Query("cursor"))
	if err != nil {
		writeError(c, err)
		return nil, 0, false
	}
	return cur, limit, true
}

// respond writes v as JSON or the error as a problem.
func respond(c *gin.Context, status int, v any, err error) {
	if err != nil {
		writeError(c, err)
		return
	}
	if status == http.StatusNoContent {
		c.Status(status)
		return
	}
	c.JSON(status, v)
}

// page maps a domain page to a DTO page.
func page[T, D any](p pagination.Page[T], f func(T) D) pagination.Page[D] {
	out := pagination.Page[D]{Items: make([]D, 0, len(p.Items)), NextCursor: p.NextCursor}
	for _, it := range p.Items {
		out.Items = append(out.Items, f(it))
	}
	return out
}

// list is a non-paged list response.
type list[T any] struct {
	Items []T `json:"items"`
}

// itemsOf wraps a non-paged list.
func itemsOf[T any](rows []T) list[T] {
	if rows == nil {
		rows = []T{}
	}
	return list[T]{Items: rows}
}

// items maps and wraps a non-paged list.
func items[T, D any](rows []T, f func(T) D) list[D] {
	out := make([]D, 0, len(rows))
	for _, r := range rows {
		out = append(out, f(r))
	}
	return list[D]{Items: out}
}
