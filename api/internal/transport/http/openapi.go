package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/product"
)

// The OpenAPI 3.1 document is generated from the operation table in
// openapi_ops.go: every route declares its request and response Go types
// (reflected into JSON Schema from the same DTO structs the handlers
// encode), its permissions, error codes, version (If-Match) and
// idempotency behaviour. openapi_test.go fails when a registered Gin route
// is missing from the table or the table lists a route that does not exist,
// and when docs/openapi.json is not the generated document.

// authMode is who may call an operation.
type authMode int

const (
	authNone        authMode = iota // public
	authUser                        // signed-in users (bearer access token)
	authUserOrToken                 // users or integration tokens
	authAdmin                       // administrators
	authCookie                      // refresh cookie + CSRF (session endpoints)
)

// ifMatchMode is how an operation uses If-Match.
type ifMatchMode int

const (
	ifMatchNone ifMatchMode = iota
	ifMatchRequired
	ifMatchOptional
)

// param is a query parameter.
type param struct {
	Name        string
	Description string
	Schema      map[string]any
}

// errSpec is an operation-specific error response.
type errSpec struct {
	Status int
	Codes  []string
}

// op describes one REST operation.
type op struct {
	Method, Path string // Gin route, e.g. /api/v1/tickets/:t
	Id           string // operationId
	Tag          string
	Summary      string
	Description  string
	Auth         authMode
	// Permission states the required role/capability (and integration
	// scopes for authUserOrToken).
	Permission string
	Query      []param
	Request    any    // zero value of the JSON request type; nil: no body
	Multipart  bool   // multipart/form-data upload (field "file")
	Response   any    // zero value of the response type; nil: 204 No Content
	Media      string // non-JSON response media type (downloads)
	Status     int    // success status
	Paged      bool   // Response is the item type of a cursor page
	List       bool   // Response is the item type of {"items": [...]}
	IfMatch    ifMatchMode
	Idempotent bool
	Errors     []errSpec
	Example    any // request example
}

func (o op) openAPIPath() string {
	return ginParam.ReplaceAllString(o.Path, "{$1}")
}

var ginParam = regexp.MustCompile(`:([A-Za-z_]+)`)

// pathParams documents path parameters by Gin name.
var pathParams = map[string]string{
	"t":          "Ticket number (e.g. `42`; `#42` URL-encoded as `%2342` also works) or ID.",
	"g":          "Group ID.",
	"u":          "User ID.",
	"c":          "Contact ID.",
	"i":          "Integration (service principal) ID.",
	"a":          "Attachment ID.",
	"m":          "Canned reply ID.",
	"w":          "Webhook endpoint ID.",
	"d":          "Delivery ID.",
	"visibility": "Draft audience: `public` or `agent`.",
	"l":          "Ticket link ID.",
	"msg":        "Message ID.",
	"name":       "Tag name (normalized).",
}

// ---- Schema reflection ----

type schemaMode int

const (
	modeResponse schemaMode = iota
	modeRequest
)

type schemaKey struct {
	t    reflect.Type
	mode schemaMode
}

type schemaGen struct {
	comps map[string]any
	names map[schemaKey]string
	owner map[string]schemaKey
}

func newSchemaGen() *schemaGen {
	return &schemaGen{comps: map[string]any{}, names: map[schemaKey]string{}, owner: map[string]schemaKey{}}
}

var (
	timeType       = reflect.TypeFor[time.Time]()
	optionalIDType = reflect.TypeFor[optionalID]()
)

// componentName derives a stable schema name from a Go type name:
// ticketDTO → Ticket, createTicketRequest → CreateTicketRequest (request
// mode), webhookBody → WebhookRequest.
func componentName(t reflect.Type, mode schemaMode) string {
	n := t.Name()
	for _, suf := range []string{"DTO", "Body", "Request"} {
		n = strings.TrimSuffix(n, suf)
	}
	r := []rune(n)
	r[0] = unicode.ToUpper(r[0])
	n = string(r)
	for _, ini := range []string{"Sla", "Smtp"} {
		if strings.HasPrefix(n, ini) {
			n = strings.ToUpper(ini) + n[len(ini):]
		}
	}
	if mode == modeRequest {
		n += "Request"
	}
	return n
}

func nullSchema(s map[string]any) map[string]any {
	if ref, ok := s["$ref"]; ok {
		return map[string]any{"anyOf": []any{map[string]any{"$ref": ref}, map[string]any{"type": "null"}}}
	}
	if t, ok := s["type"].(string); ok {
		out := map[string]any{}
		for k, v := range s {
			out[k] = v
		}
		out["type"] = []any{t, "null"}
		return out
	}
	return map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
}

// schema returns the JSON Schema of t (a $ref for named structs).
func (g *schemaGen) schema(t reflect.Type, mode schemaMode) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == optionalIDType:
		return map[string]any{"type": []any{"string", "null"}, "description": "An ID, or null to clear."}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return nullSchema(g.schema(t.Elem(), mode))
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int16, reflect.Int8, reflect.Uint, reflect.Uint32, reflect.Uint16, reflect.Uint8:
		return map[string]any{"type": "integer"}
	case reflect.Int64, reflect.Uint64:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": g.schema(t.Elem(), mode)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schema(t.Elem(), mode)}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		if t.Name() == "" || strings.Contains(t.Name(), "[") {
			return g.structSchema(t, mode)
		}
		key := schemaKey{t, mode}
		name, ok := g.names[key]
		if !ok {
			name = componentName(t, mode)
			if prev, dup := g.owner[name]; dup && prev != key {
				panic(fmt.Sprintf("openapi: schema name %s used by %v and %v", name, prev.t, t))
			}
			g.names[key], g.owner[name] = name, key
			g.comps[name] = map[string]any{} // placeholder for recursion
			g.comps[name] = g.structSchema(t, mode)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	panic("openapi: unsupported type " + t.String())
}

// structSchema reflects a struct's JSON fields. Embedded structs are
// flattened as encoding/json does. Responses list every field without
// omitempty as required (the server always sends it); requests list only
// fields tagged required:"true". Tags enum, minimum, maximum, format and
// doc refine a property.
func (g *schemaGen) structSchema(t reflect.Type, mode schemaMode) map[string]any {
	props := map[string]any{}
	var required []string
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" || (!f.IsExported() && !f.Anonymous) {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				walk(ft)
				continue
			}
			if name == "" {
				name = f.Name
			}
			s := g.schema(f.Type, mode)
			s = refine(s, f.Tag)
			props[name] = s
			omit := strings.Contains(opts, "omitempty")
			if (mode == modeResponse && !omit) || (mode == modeRequest && f.Tag.Get("required") == "true") {
				required = append(required, name)
			}
		}
	}
	walk(t)
	out := map[string]any{"type": "object", "properties": props}
	if mode == modeRequest {
		// Strict decoding: unknown members are rejected with 422.
		out["additionalProperties"] = false
	}
	if len(required) > 0 {
		slices.Sort(required)
		out["required"] = required
	}
	return out
}

// refine applies schema tags to a property (inside anyOf for nullable refs).
func refine(s map[string]any, tag reflect.StructTag) map[string]any {
	set := func(k string, v any) {
		target := s
		if alts, ok := s["anyOf"].([]any); ok {
			if inner, ok := alts[0].(map[string]any); ok && inner["$ref"] == nil {
				target = inner
			}
		}
		if items, ok := target["items"].(map[string]any); ok && target["type"] == "array" && (k == "enum" || k == "minimum" || k == "maximum") {
			target = items
		}
		target[k] = v
	}
	if e := tag.Get("enum"); e != "" {
		vals := []any{}
		for _, v := range strings.Split(e, ",") {
			if n, err := strconv.Atoi(v); err == nil {
				vals = append(vals, n)
			} else {
				vals = append(vals, v)
			}
		}
		if ty, ok := s["type"].([]any); ok && slices.Contains(ty, any("null")) {
			vals = append(vals, nil)
		}
		set("enum", vals)
	}
	for _, k := range []string{"minimum", "maximum"} {
		if v := tag.Get(k); v != "" {
			n, _ := strconv.Atoi(v)
			set(k, n)
		}
	}
	if v := tag.Get("format"); v != "" {
		set("format", v)
	}
	if v := tag.Get("doc"); v != "" {
		if s["$ref"] != nil || s["anyOf"] != nil {
			return map[string]any{"allOf": []any{s}, "description": v}
		}
		s["description"] = v
	}
	return s
}

// ---- Document ----

func problemResponse(desc string, codes []string) map[string]any {
	d := desc
	if len(codes) > 0 {
		d += " Codes: `" + strings.Join(codes, "`, `") + "`."
	}
	return map[string]any{"description": d, "content": map[string]any{
		"application/problem+json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Problem"}}}}
}

var statusText = map[int]string{
	400: "Malformed request (invalid JSON, If-Match or header).",
	401: "Not authenticated, or the token is invalid, expired or revoked.",
	403: "The operation is visible but not permitted for this role or integration scope.",
	404: "Not found, or not visible to the caller (existence is hidden).",
	405: "Method not allowed.",
	409: "Conflict with the current state (versions, business rules, idempotency).",
	410: "The resource is no longer available.",
	413: "The request body or upload is too large.",
	415: "Unsupported content type.",
	422: "Valid JSON that fails field rules; `errors` lists each field.",
	428: "If-Match is required.",
	429: "Too many requests; see Retry-After.",
	503: "The installation is not ready.",
}

func (o op) responses(g *schemaGen) map[string]any {
	res := map[string]any{}
	status := o.Status
	if status == 0 {
		status = http.StatusOK
		if o.Response == nil && o.Media == "" {
			status = http.StatusNoContent
		}
	}
	ok := map[string]any{"description": http.StatusText(status)}
	switch {
	case o.Media != "":
		ok["content"] = map[string]any{o.Media: map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}
	case o.Response != nil:
		item := g.schema(reflect.TypeOf(o.Response), modeResponse)
		var s map[string]any
		switch {
		case o.Paged:
			s = map[string]any{"type": "object", "required": []string{"items", "nextCursor"}, "properties": map[string]any{
				"items":      map[string]any{"type": "array", "items": item},
				"nextCursor": map[string]any{"type": []any{"string", "null"}, "description": "Opaque cursor for the next page; null on the last page."},
			}}
		case o.List:
			s = map[string]any{"type": "object", "required": []string{"items"}, "properties": map[string]any{
				"items": map[string]any{"type": "array", "items": item}}}
		default:
			s = item
		}
		ok["content"] = map[string]any{"application/json": map[string]any{"schema": s}}
	}
	res[strconv.Itoa(status)] = ok
	if o.Method == http.MethodPost && status == http.StatusOK && o.Id == "upsertContact" {
		res["201"] = map[string]any{"description": "Created (a new contact).", "content": ok["content"]}
	}

	codes := map[int][]string{}
	add := func(s int, c ...string) { codes[s] = append(codes[s], c...) }
	if o.Auth != authNone {
		add(401, "authenticationRequired", "invalidToken")
	}
	if o.Auth == authAdmin {
		add(403, "forbidden")
	}
	if (o.Auth == authUser || o.Auth == authAdmin || o.Auth == authUserOrToken) && !mfaEnrolmentRoutes[o.Path] {
		add(403, "mfaEnrolmentRequired")
	}
	if o.Auth == authUser {
		add(403, "userRequired")
	}
	if o.Request != nil || o.Multipart {
		add(400, "malformedJson")
		add(413, "bodyTooLarge")
		add(422, "validationFailed")
	}
	if len(o.Query) > 0 || o.Paged {
		add(422, "validationFailed")
	}
	if o.IfMatch == ifMatchRequired {
		add(428, "preconditionRequired")
	}
	if o.IfMatch != ifMatchNone {
		add(400, "invalidIfMatch")
		add(409, "versionConflict")
	}
	if o.Idempotent {
		add(409, "idempotencyKeyReused", "idempotencyInProgress")
	}
	if _, limited := routeLimit[o.Method+" "+o.Path]; limited {
		add(429, "rateLimited")
	}
	for _, e := range o.Errors {
		add(e.Status, e.Codes...)
	}
	for s, cs := range codes {
		slices.Sort(cs)
		res[strconv.Itoa(s)] = problemResponse(statusText[s], slices.Compact(cs))
	}
	return res
}

func (o op) operation(g *schemaGen) map[string]any {
	desc := o.Description
	if o.Permission != "" {
		desc = strings.TrimSpace(desc + "\n\n**Permission:** " + o.Permission)
	}
	if o.Idempotent {
		desc += "\n\n**Idempotency:** send `Idempotency-Key` to make retries safe; the same key with a different body returns `409 idempotencyKeyReused`."
	}
	if o.IfMatch == ifMatchRequired {
		desc += "\n\n**Versioning:** send `If-Match` with the resource's current `version`; a stale version returns `409 versionConflict`."
	}
	out := map[string]any{"operationId": o.Id, "summary": o.Summary, "tags": []string{o.Tag}, "description": strings.TrimSpace(desc),
		"responses": o.responses(g)}
	switch o.Auth {
	case authNone:
		out["security"] = []any{}
	case authCookie:
		out["security"] = []any{map[string]any{"refreshCookie": []any{}, "csrfHeader": []any{}}}
	case authUserOrToken:
		out["security"] = []any{map[string]any{"userBearer": []any{}}, map[string]any{"integrationToken": []any{}}}
	default:
		out["security"] = []any{map[string]any{"userBearer": []any{}}}
	}
	var params []any
	for _, m := range ginParam.FindAllStringSubmatch(o.Path, -1) {
		params = append(params, map[string]any{"name": m[1], "in": "path", "required": true,
			"description": pathParams[m[1]], "schema": map[string]any{"type": "string"}})
	}
	if o.Paged {
		params = append(params, map[string]any{"$ref": "#/components/parameters/Cursor"}, map[string]any{"$ref": "#/components/parameters/Limit"})
	}
	for _, q := range o.Query {
		s := q.Schema
		if s == nil {
			s = map[string]any{"type": "string"}
		}
		params = append(params, map[string]any{"name": q.Name, "in": "query", "description": q.Description, "schema": s})
	}
	switch o.IfMatch {
	case ifMatchRequired:
		params = append(params, map[string]any{"$ref": "#/components/parameters/IfMatch"})
	case ifMatchOptional:
		params = append(params, map[string]any{"$ref": "#/components/parameters/IfMatchOptional"})
	}
	if o.Idempotent {
		params = append(params, map[string]any{"$ref": "#/components/parameters/IdempotencyKey"})
	}
	if len(params) > 0 {
		out["parameters"] = params
	}
	switch {
	case o.Multipart:
		out["requestBody"] = map[string]any{"required": true, "content": map[string]any{"multipart/form-data": map[string]any{
			"schema": map[string]any{"type": "object", "required": []string{"file"}, "properties": map[string]any{
				"file": map[string]any{"type": "string", "format": "binary"}}}}}}
	case o.Request != nil:
		media := map[string]any{"schema": g.schema(reflect.TypeOf(o.Request), modeRequest)}
		if o.Example != nil {
			media["example"] = o.Example
		}
		out["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": media}}
	}
	return out
}

const apiDescription = `REST API of ` + product.Name + `. All routes are under ` + "`/api/v1`" + `; bodies are JSON (UTF-8),
timestamps are UTC RFC 3339, IDs are opaque strings.

**Authentication.** Staff send ` + "`Authorization: Bearer <access token>`" + ` (a short-lived JWT from
` + "`POST /auth/login`" + ` or ` + "`/auth/refresh`" + `). External systems send ` + "`Authorization: Bearer kase_live_…`" + ` (an
API token, created by an administrator). There is one kind of token; its creator picks the scopes: ` + "`tickets:read`" + `,
` + "`tickets:create`" + `, ` + "`tickets:replyAsRequester`" + ` (public replies as the requester), ` + "`tickets:comment`" + ` (public
replies as staff, under the token's name), ` + "`tickets:internal`" + ` (agent notes), ` + "`tickets:write`" + ` (status, priority, tags,
group, assignee), ` + "`tickets:all`" + ` (every ticket scope except replying as the requester), ` + "`contacts:read`" + `,
` + "`contacts:write`" + `. A **staff token** holds ` + "`tickets:comment`" + `, ` + "`tickets:internal`" + `, ` + "`tickets:write`" + ` or
` + "`tickets:all`" + ` and acts as support staff under its own name (a bot): staff ticket view, groups and agents. Any other
token is a **requester-side token**: it never sees assignees or staff working data. Agent notes need ` + "`tickets:internal`" + `.
A token never exceeds its scopes or what a human agent may do (no claiming or administration), and every token write is audited
with the token and the scope that allowed it. The same tokens work at ` + "`/mcp`" + ` (Model Context Protocol). Refresh and logout use the ` + "`kase_refresh`" + ` cookie plus the ` + "`X-CSRF-Token`" + ` header (double
submit with the ` + "`kase_csrf`" + ` cookie) and an ` + "`Origin`" + ` header.

**Errors** are ` + "`application/problem+json`" + ` (schema ` + "`Problem`" + `) with a stable ` + "`code`" + `, the
` + "`requestId`" + ` and, for 422, per-field ` + "`errors`" + `. Inaccessible objects are 404 so their existence is hidden.

**Pagination.** Lists return ` + "`{items, nextCursor}`" + `; pass ` + "`cursor`" + ` (opaque) and ` + "`limit`" + ` (1–100,
default 25). Access filters apply before limits and counts.

**Versions.** Mutations of versioned resources require ` + "`If-Match: <version>`" + `; a stale version returns
` + "`409 versionConflict`" + ` and nothing changes.

**Idempotency.** Create operations accept ` + "`Idempotency-Key`" + ` (1–200 printable ASCII). A retry with the same key and
body replays the stored response (header ` + "`Idempotent-Replayed: true`" + `) after re-checking access; the same key with a
different body returns ` + "`409 idempotencyKeyReused`" + `. Keys are scoped to the caller and route.`

var tagDescriptions = map[string]string{
	"Meta":          "API identity and this document.",
	"Auth":          "Sign-in, rotating refresh tokens and sign-out (staff only).",
	"Me":            "The signed-in user's profile, preferences and projects.",
	"Settings":      "Installation settings (administrators; secrets are write-only).",
	"Users":         "Staff accounts (administrators).",
	"Customers":     "Requester contacts.",
	"Projects":      "Projects, their statuses and request types.",
	"Groups":        "Groups inside a project and their members.",
	"Members":       "Project memberships and roles.",
	"Integrations":  "Project-scoped integration credentials (service principals).",
	"Tickets":       "Group-first tickets: create, claim, assign, transition, watch.",
	"Messages":      "Public replies, agent notes, drafts and attachments.",
	"Search":        "Authorized full-text search with audience-scoped snippets.",
	"Productivity":  "Tags (admin-managed catalog) and canned replies.",
	"SLA":           "The SLA (first-response targets) and per-ticket SLA cycles.",
	"Reports":       "Bounded live reports.",
	"Notifications": "Email and webhook delivery records (administrators).",
	"Webhooks":      "Outbound webhook endpoints of a project.",
	"Audit":         "The audit log (administrators; field names and identifiers only, never bodies or secrets).",
}

// buildOpenAPI assembles the document from the operation table.
func buildOpenAPI(ops []op) map[string]any {
	g := newSchemaGen()
	paths := map[string]any{}
	tags := []string{}
	for _, o := range ops {
		p, _ := paths[o.openAPIPath()].(map[string]any)
		if p == nil {
			p = map[string]any{}
			paths[o.openAPIPath()] = p
		}
		p[strings.ToLower(o.Method)] = o.operation(g)
		if !slices.Contains(tags, o.Tag) {
			tags = append(tags, o.Tag)
		}
	}
	g.schema(reflect.TypeFor[fault.Problem](), modeResponse)
	tagList := make([]any, 0, len(tags))
	for _, t := range tags {
		tagList = append(tagList, map[string]any{"name": t, "description": tagDescriptions[t]})
	}
	return map[string]any{
		"openapi":           "3.1.0",
		"jsonSchemaDialect": "https://spec.openapis.org/oas/3.1/dialect/base",
		"info": map[string]any{
			"title":       product.Name + " REST API",
			"version":     "v1",
			"description": apiDescription,
		},
		"servers": []any{map[string]any{"url": "/", "description": "This installation"}},
		"tags":    tagList,
		"paths":   paths,
		"components": map[string]any{
			"schemas": g.comps,
			"securitySchemes": map[string]any{
				"userBearer": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT",
					"description": "Staff access token (EdDSA JWT, typically 10 minutes) from /auth/login or /auth/refresh. Checked against the current session and user on every request."},
				"integrationToken": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "kase_live_…",
					"description": "API token. Its scopes bound every operation; revocation applies immediately."},
				"refreshCookie": map[string]any{"type": "apiKey", "in": "cookie", "name": "kase_refresh",
					"description": "HttpOnly rotating refresh token (one use; reuse revokes the session)."},
				"csrfHeader": map[string]any{"type": "apiKey", "in": "header", "name": "X-CSRF-Token",
					"description": "Must equal the kase_csrf cookie and match the session."},
			},
			"parameters": map[string]any{
				"Cursor": map[string]any{"name": "cursor", "in": "query", "description": "Opaque cursor from the previous page's nextCursor.",
					"schema": map[string]any{"type": "string", "maxLength": 1024}},
				"Limit": map[string]any{"name": "limit", "in": "query", "description": "Page size.",
					"schema": map[string]any{"type": "integer", "minimum": 1, "maximum": pagination.MaxLimit, "default": pagination.DefaultLimit}},
				"IfMatch": map[string]any{"name": "If-Match", "in": "header", "required": true,
					"description": "The version being changed: a number, or an entity tag such as \"3\" or W/\"3\".",
					"schema":      map[string]any{"type": "string"}, "example": "3"},
				"IfMatchOptional": map[string]any{"name": "If-Match", "in": "header", "required": false,
					"description": "Optional version precondition.", "schema": map[string]any{"type": "string"}, "example": "3"},
				"IdempotencyKey": map[string]any{"name": "Idempotency-Key", "in": "header", "required": false,
					"description": "Makes the create safe to retry (1–200 printable ASCII characters, retained 24 hours).",
					"schema":      map[string]any{"type": "string", "minLength": 1, "maxLength": 200, "pattern": "^[\\x21-\\x7e]+$"},
					"example":     "4f1c2b8e-create-ticket"},
			},
		},
	}
}

var (
	openAPIOnce sync.Once
	openAPIJSON []byte
)

// OpenAPIDocument returns the generated OpenAPI 3.1 document as indented
// JSON (stable key order).
func OpenAPIDocument() []byte {
	openAPIOnce.Do(func() {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(buildOpenAPI(operations())); err != nil {
			panic("openapi: " + err.Error())
		}
		openAPIJSON = buf.Bytes()
	})
	return openAPIJSON
}

func serveOpenAPI(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=300")
	c.Data(http.StatusOK, "application/json; charset=utf-8", OpenAPIDocument())
}

// RouteAuth is the access class of one documented REST route. It is exported
// for the route authorization tests, which enumerate every operation.
type RouteAuth struct {
	Method, Path string
	// Public routes need no credential; Cookie routes need the refresh
	// cookie and CSRF token; AdminOnly routes need an administrator;
	// Integrations routes also accept integration tokens.
	Public, Cookie, AdminOnly, Integrations bool
	// Multipart is an upload route; Example is the documented request
	// example (nil when none).
	Multipart bool
	Example   any
}

// RouteAuthTable lists the access class of every documented REST route.
func RouteAuthTable() []RouteAuth {
	ops := operations()
	out := make([]RouteAuth, 0, len(ops))
	for _, o := range ops {
		out = append(out, RouteAuth{Method: o.Method, Path: o.Path, Public: o.Auth == authNone, Cookie: o.Auth == authCookie,
			AdminOnly: o.Auth == authAdmin, Integrations: o.Auth == authUserOrToken, Multipart: o.Multipart, Example: o.Example})
	}
	return out
}
