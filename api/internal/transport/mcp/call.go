package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/package/fault"
)

// call is the per-invocation context of one tool call. Tools record the
// project and ticket they resolved so the audit event can name them.
type call struct {
	p         access.Principal
	projectID string
	ticketID  string
	// scope is the last resolved scope; its audit actor names the token
	// scopes the call used.
	scope *access.ProjectScope
}

// scope authorizes the caller.
func (h *Handler) scope(ctx context.Context, c *call) (*access.ProjectScope, error) {
	s, err := h.d.Access.Scope(ctx, c.p)
	if err != nil {
		return nil, err
	}
	c.projectID, c.scope = s.Project.Id, s
	return s, nil
}

// toolFunc is a tool body: it returns the structured result or an error.
type toolFunc[In any] func(ctx context.Context, c *call, in In) (any, error)

// addTool registers a tool whose input schema is inferred from In and then
// adjusted by patch. The wrapper resolves the principal, bounds the call,
// maps errors to problem results, and writes the audit event and log line.
func addTool[In any](h *Handler, t *sdk.Tool, patch func(*jsonschema.Schema), fn toolFunc[In]) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic("mcp: schema for " + t.Name + ": " + err.Error())
	}
	if patch != nil {
		patch(schema)
	}
	t.InputSchema = schema
	sdk.AddTool(h.server, t, func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		start := time.Now()
		p, requestID := caller(req)
		if p.Id == "" {
			return problem(fault.Unauthenticated("authenticationRequired", "Provide an API token."), ""), nil, nil
		}
		ctx = audit.WithRequestID(ctx, requestID)
		ctx, cancel := context.WithTimeout(ctx, h.d.CallTimeout)
		defer cancel()
		c := &call{p: p}
		out, err := fn(ctx, c, in)
		if errors.Is(err, context.DeadlineExceeded) {
			err = fault.New(fault.KindUnavailable, "toolTimeout", "The tool call took too long and was stopped.")
		}
		h.record(context.WithoutCancel(ctx), t.Name, c, req, err, time.Since(start))
		if err != nil {
			return problem(err, requestID), nil, nil
		}
		return nil, out, nil
	})
}

// caller returns the principal the REST adapter authenticated.
func caller(req *sdk.CallToolRequest) (access.Principal, string) {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return access.Principal{}, ""
	}
	p, _ := req.Extra.TokenInfo.Extra[extraPrincipal].(access.Principal)
	id, _ := req.Extra.TokenInfo.Extra[extraRequestID].(string)
	return p, id
}

// problem renders err as an MCP tool error carrying the REST problem body
// (stable code, field errors); internal causes are never exposed.
func problem(err error, requestID string) *sdk.CallToolResult {
	b, _ := json.Marshal(fault.ToProblem(err, requestID))
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}
}

// record writes the tool-call audit event and a structured log line.
// Argument names are recorded, never their values (bodies, contact data).
func (h *Handler) record(ctx context.Context, tool string, c *call, req *sdk.CallToolRequest, err error, took time.Duration) {
	var args map[string]json.RawMessage
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		_ = json.Unmarshal(req.Params.Arguments, &args)
	}
	names := slices.Sorted(maps.Keys(args))
	actor := c.p.AuditActor()
	if c.scope != nil {
		actor = c.scope.AuditActor()
	}
	ev := audit.New(ctx, actor, audit.MCPToolCalled, "mcpTool/"+tool, h.d.Clock.Now())
	ev.WorkspaceId, ev.ProjectId, ev.TicketId, ev.Changes = c.p.WorkspaceId, c.projectID, c.ticketID, names
	level, outcome, code := slog.LevelInfo, "success", ""
	if err != nil {
		fe := fault.As(err)
		outcome, code = "failure", fe.Code
		ev.Outcome, ev.Reason = "failure", fe.Code
		if fe.Kind.Status() >= 500 {
			level = slog.LevelError
		}
	}
	if h.d.Audit != nil {
		if aerr := h.d.Audit.Append(ctx, ev); aerr != nil {
			h.log.Warn("mcp audit append failed", "tool", tool, "error", aerr.Error())
		}
	}
	attrs := []any{"requestId", ev.RequestId, "tool", tool, "principalKind", string(c.p.Kind), "principalId", c.p.Id,
		"projectId", c.projectID, "ticketId", c.ticketID, "arguments", names, "outcome", outcome, "durationMs", took.Milliseconds()}
	if code != "" {
		attrs = append(attrs, "code", code)
	}
	if level == slog.LevelError {
		attrs = append(attrs, "error", err.Error())
	}
	h.log.Log(ctx, level, "mcp tool call", attrs...)
}

var validIdempotencyKey = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)

// idempotent runs a mutation under an optional idempotency key, with the
// same semantics as the REST Idempotency-Key header: the key is scoped to
// the principal and the tool, a retry with identical arguments replays the
// stored result, different arguments fail with idempotencyKeyReused.
// recheck runs before a replay so a stored result never outlives the
// caller's current access. Commands that record the key inside their own
// transaction (ticket creation, messages) find it in the context.
func (h *Handler) idempotent(ctx context.Context, c *call, tool, key string, args any, recheck func(context.Context) error, run func(context.Context) (any, error)) (any, error) {
	if key == "" || h.d.Idempotency == nil {
		return run(ctx)
	}
	if !validIdempotencyKey.MatchString(key) {
		return nil, fault.Field("idempotencyKey", "invalidFormat", "Use 1 to 200 printable ASCII characters.")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte("mcp "+tool+"\n"), raw...))
	scope := string(c.p.Kind) + ":" + c.p.Id + ":mcp " + tool
	rec, err := h.d.Idempotency.Begin(ctx, scope, key, hex.EncodeToString(sum[:]), h.d.Clock.Now())
	switch {
	case errors.Is(err, idempotency.ErrMismatch):
		return nil, fault.Conflict("idempotencyKeyReused", "This idempotencyKey was used with different arguments.")
	case errors.Is(err, idempotency.ErrInProgress):
		return nil, fault.Conflict("idempotencyInProgress", "A call with this idempotencyKey is still being processed.")
	case err != nil:
		return nil, err
	}
	if rec != nil {
		if err := recheck(ctx); err != nil {
			return nil, err
		}
		if len(rec.Body) == 0 {
			// The stored result was removed (the message it contained was
			// redacted): the call completed, but is not replayed.
			return nil, fault.Conflict("idempotencyKeyUsed", "This call already completed; its result is no longer available.")
		}
		return json.RawMessage(rec.Body), nil
	}
	out, err := run(idempotency.WithRef(ctx, idempotency.Ref{Scope: scope, Key: key}))
	// The outcome is recorded even if the client has gone meanwhile.
	octx, cancel := idempotency.OutcomeContext(ctx)
	defer cancel()
	if err != nil {
		if aerr := h.d.Idempotency.Abandon(octx, scope, key); aerr != nil {
			h.log.Warn("idempotency abandon failed", "error", aerr.Error())
		}
		return nil, err
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if err := h.d.Idempotency.Complete(octx, scope, key, idempotency.Record{Status: 200, Body: body}, h.d.Clock.Now()); err != nil {
		h.log.Warn("idempotency completion failed", "error", err.Error())
	}
	return out, nil
}
