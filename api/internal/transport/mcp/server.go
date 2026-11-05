// Package mcp is the Model Context Protocol adapter: a Streamable HTTP
// endpoint (official Go SDK, stateless mode) whose tools call the same
// application services as the REST API. It holds no ticket logic of its
// own: every tool call resolves a fresh access.ProjectScope from MongoDB and
// the services make every authorization, validation, state and audience
// decision exactly as they do for REST.
//
// Authentication happens before the request reaches this package (the REST
// adapter's bearer middleware, see transport/http.MountMCP); the SDK only
// receives the already resolved principal through auth.TokenInfo.
package mcp

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/organisations"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/product"
	"github.com/arksnorman/open-kase/api/package/ratelimit"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// ProtocolVersions are the MCP revisions this endpoint negotiates, newest
// first. 2026-07-28 is the pinned target revision of go-sdk v1.8.0; older
// Streamable HTTP revisions stay available for existing clients. 2024-11-05
// (the HTTP+SSE transport) is not offered.
var ProtocolVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// Defaults for Deps fields left at zero.
const (
	DefaultCallTimeout   = 30 * time.Second
	DefaultMaxBodyBytes  = validation.DefaultMaxBodyBytes
	DefaultPerMinute     = 120
	DefaultBurst         = 30
	DefaultMaxPrincipals = 10_000
)

// AuditLog appends standalone audit events (mongo AuditRepository).
type AuditLog interface {
	Append(ctx context.Context, ev audit.Event) error
}

// Deps are the adapter's collaborators.
type Deps struct {
	Logger        *slog.Logger
	Access        *access.Service
	Tickets       *tickets.Service
	Projects      *projects.Service
	Customers     *customers.Service
	Macros        *macros.Service
	HoldReasons   *holdreasons.Service
	Organisations *organisations.Service
	Workflow      *statuses.Catalog
	Idempotency   idempotency.Store
	Audit         AuditLog
	Clock         clock.Clock
	// BaseURL returns the configured public base URL; its origin is the one
	// browser origin accepted besides loopback development origins.
	BaseURL func() string

	// CallTimeout bounds one tool call. MaxBodyBytes bounds one HTTP body.
	CallTimeout  time.Duration
	MaxBodyBytes int64
	// PerMinute/Burst set the per-principal request rate; MaxPrincipals
	// bounds the limiter's memory.
	PerMinute     int
	Burst         int
	MaxPrincipals int
}

// Handler is the /mcp http.Handler.
type Handler struct {
	d       Deps
	log     *slog.Logger
	server  *sdk.Server
	http    http.Handler
	limiter *ratelimit.Limiter
}

// New builds the MCP server, registers the tools and wraps the SDK's
// Streamable HTTP handler.
func New(d Deps) *Handler {
	if d.CallTimeout <= 0 {
		d.CallTimeout = DefaultCallTimeout
	}
	if d.MaxBodyBytes <= 0 {
		d.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if d.PerMinute <= 0 {
		d.PerMinute = DefaultPerMinute
	}
	if d.Burst <= 0 {
		d.Burst = DefaultBurst
	}
	if d.MaxPrincipals <= 0 {
		d.MaxPrincipals = DefaultMaxPrincipals
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	log := d.Logger.With("component", "mcp")
	h := &Handler{d: d, log: log, limiter: ratelimit.New(d.PerMinute, d.Burst, d.MaxPrincipals, d.Clock.Now)}
	h.server = sdk.NewServer(&sdk.Implementation{Name: product.Slug, Title: product.Name, Version: product.Version}, &sdk.ServerOptions{
		Instructions: "Support ticket tools for " + product.Name + ". Every call is authorized as the token's principal " +
			"with the token's scopes; tools return only what the same principal can see and do through the REST API. " +
			"Public replies reach the requester; agent notes are staff-only and need audience \"agent\".",
		Logger:                    log,
		SupportedProtocolVersions: ProtocolVersions,
	})
	h.registerTools()

	streamable := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return h.server }, &sdk.StreamableHTTPOptions{
		// Stateless: no Mcp-Session-Id, no sticky routing; every POST is
		// authenticated and handled on its own. GET/DELETE answer 405.
		Stateless:    true,
		JSONResponse: true,
		Logger:       log,
		// The REST adapter's Origin check and mandatory bearer token replace
		// the SDK's loopback Host heuristic, which would reject requests
		// forwarded by a reverse proxy on the same host.
		DisableLocalhostProtection:   true,
		MaxRequestBodyBytes:          d.MaxBodyBytes,
		PropagateRequestCancellation: true,
	})
	// The principal is already authenticated; RequireBearerToken only hands
	// it to tool handlers as TokenInfo (it never sees an unauthenticated
	// request in production).
	h.http = sdkauth.RequireBearerToken(verifyFromContext, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(streamable)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.http.ServeHTTP(w, r) }

const (
	extraPrincipal = "principal"
	extraRequestID = "requestId"
)

// verifyFromContext turns the principal stored by the REST adapter into
// SDK token info. A request without one is rejected.
func verifyFromContext(ctx context.Context, _ string, r *http.Request) (*sdkauth.TokenInfo, error) {
	p, ok := access.PrincipalFrom(r.Context())
	if !ok {
		return nil, sdkauth.ErrInvalidToken
	}
	return &sdkauth.TokenInfo{UserID: string(p.Kind) + ":" + p.Id, Scopes: p.Scopes,
		Extra: map[string]any{extraPrincipal: p, extraRequestID: audit.RequestID(r.Context())}}, nil
}

// CheckOrigin enforces the MCP transport's Origin rule (DNS-rebinding
// protection). Requests without Origin (non-browser clients) pass. A
// browser Origin must equal the configured public base URL's origin, or be
// a loopback origin while the request itself targets a loopback host.
// Unlike the REST cookie check, "same Host" is not enough: under DNS
// rebinding the attacker controls both.
func (h *Handler) CheckOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	reject := fault.Forbidden("originRejected", "The request origin is not allowed to use the MCP endpoint.")
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return reject
	}
	if h.d.BaseURL != nil {
		if base := h.d.BaseURL(); base != "" {
			if b, err := url.Parse(base); err == nil && strings.EqualFold(b.Scheme, u.Scheme) && strings.EqualFold(b.Host, u.Host) {
				return nil
			}
		}
	}
	if isLoopback(u.Hostname()) && isLoopback(hostOnly(r.Host)) {
		return nil
	}
	return reject
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Admit lets an authenticated principal use the endpoint (any user or API
// token; every tool checks the same scopes as REST) and rate limits it.
func (h *Handler) Admit(_ context.Context, p access.Principal) error {
	if p.Kind != access.KindIntegration && p.Kind != access.KindUser {
		return fault.Forbidden("forbidden", "This principal cannot use the MCP endpoint.")
	}
	if wait := h.limiter.Allow(string(p.Kind) + ":" + p.Id); wait > 0 {
		return fault.RateLimited("rateLimited", "Too many MCP requests for this credential. Retry later.", wait)
	}
	return nil
}
