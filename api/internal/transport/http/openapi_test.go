package http

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/arksnorman/open-kase/api/package/logger"
)

// fullRouter registers every route: each pointer-typed service is set to a
// zero value (handlers are not invoked), so routes registered only when a
// service is configured are included too — including services added later.
func fullRouter(t *testing.T) []string {
	t.Helper()
	var s Services
	v := reflect.ValueOf(&s).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Pointer && f.CanSet() {
			f.Set(reflect.New(f.Type().Elem()))
		}
	}
	r, err := NewRouter(Deps{Logger: logger.Discard(), Readiness: NewReadiness(logger.Discard()), Services: s})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, rt := range r.Routes() {
		if strings.HasPrefix(rt.Path, "/api/v1/") {
			out = append(out, rt.Method+" "+rt.Path)
		}
	}
	slices.Sort(out)
	return out
}

// TestOpenAPICoversEveryRoute is the drift check between the Gin router and
// the OpenAPI operation table.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	routes := fullRouter(t)
	var documented []string
	ids := map[string]bool{}
	for _, o := range operations() {
		key := o.Method + " " + o.Path
		if slices.Contains(documented, key) {
			t.Errorf("operation listed twice: %s", key)
		}
		if ids[o.Id] {
			t.Errorf("duplicate operationId %s", o.Id)
		}
		ids[o.Id] = true
		documented = append(documented, key)
		if o.Summary == "" || o.Tag == "" || o.Permission == "" {
			t.Errorf("%s: summary, tag and permission are required", key)
		}
	}
	slices.Sort(documented)
	for _, r := range routes {
		if !slices.Contains(documented, r) {
			t.Errorf("route %s is registered but missing from the OpenAPI operations (openapi_ops.go)", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(routes, d) {
			t.Errorf("OpenAPI operation %s has no registered route", d)
		}
	}
	if len(routes) < 100 {
		t.Fatalf("only %d routes registered; the full router was not built", len(routes))
	}
}

const committedSpec = "../../../../docs/openapi.json"

// TestOpenAPIDocumentIsCommitted keeps docs/openapi.json identical to the
// generated document. Regenerate with `open-kase openapi --out
// docs/openapi.json` or OPEN_KASE_UPDATE_OPENAPI=1 go test.
func TestOpenAPIDocumentIsCommitted(t *testing.T) {
	doc := OpenAPIDocument()
	if os.Getenv("OPEN_KASE_UPDATE_OPENAPI") == "1" {
		if err := os.WriteFile(committedSpec, doc, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(committedSpec)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate it)", committedSpec, err)
	}
	if !bytes.Equal(got, doc) {
		t.Fatalf("docs/openapi.json is stale; run `open-kase openapi --out docs/openapi.json` (or OPEN_KASE_UPDATE_OPENAPI=1 go test ./internal/transport/http/)")
	}
}

func collectRefs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "$ref" {
				*out = append(*out, val.(string))
			}
			collectRefs(val, out)
		}
	case []any:
		for _, val := range x {
			collectRefs(val, out)
		}
	}
}

func TestOpenAPIDocumentIsWellFormed(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(OpenAPIDocument(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("version %v", doc["openapi"])
	}
	comps := doc["components"].(map[string]any)
	var refs []string
	collectRefs(doc, &refs)
	for _, ref := range refs {
		parts := strings.Split(strings.TrimPrefix(ref, "#/components/"), "/")
		section, _ := comps[parts[0]].(map[string]any)
		if len(parts) != 2 || section[parts[1]] == nil {
			t.Errorf("unresolved $ref %s", ref)
		}
	}
	for path, item := range doc["paths"].(map[string]any) {
		for method, raw := range item.(map[string]any) {
			o := raw.(map[string]any)
			if o["security"] == nil || o["responses"] == nil || o["operationId"] == nil {
				t.Errorf("%s %s lacks security, responses or operationId", method, path)
			}
			for _, p := range strings.Split(path, "/") {
				if !strings.HasPrefix(p, "{") {
					continue
				}
				name := strings.Trim(p, "{}")
				found := false
				for _, prm := range o["parameters"].([]any) {
					if m := prm.(map[string]any); m["in"] == "path" && m["name"] == name {
						found = true
					}
				}
				if !found {
					t.Errorf("%s %s: path parameter %s undocumented", method, path, name)
				}
			}
		}
	}
	// Request examples use only documented fields.
	schemas0 := comps["schemas"].(map[string]any)
	for path, item := range doc["paths"].(map[string]any) {
		for method, raw := range item.(map[string]any) {
			rb, _ := raw.(map[string]any)["requestBody"].(map[string]any)
			if rb == nil {
				continue
			}
			media, _ := rb["content"].(map[string]any)["application/json"].(map[string]any)
			ex, _ := media["example"].(map[string]any)
			if ex == nil {
				continue
			}
			ref, _ := media["schema"].(map[string]any)["$ref"].(string)
			props := schemas0[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)["properties"].(map[string]any)
			for k := range ex {
				if props[k] == nil {
					t.Errorf("%s %s: example field %q is not in the request schema", method, path, k)
				}
			}
		}
	}
	// Key schemas carry the contract's audience and pagination semantics.
	schemas := comps["schemas"].(map[string]any)
	for _, name := range []string{"Ticket", "Message", "Problem", "SearchPage", "CreateTicketRequest"} {
		if schemas[name] == nil {
			t.Errorf("schema %s missing", name)
		}
	}
	secs := comps["securitySchemes"].(map[string]any)
	if secs["userBearer"] == nil || secs["integrationToken"] == nil {
		t.Fatal("auth schemes missing")
	}
}

// TestOpenAPIErrorCodesExist fails when the table documents an error code
// that no Go source produces.
func TestOpenAPIErrorCodesExist(t *testing.T) {
	var src strings.Builder
	err := filepath.WalkDir("../../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "openapi_ops.go") ||
			strings.HasSuffix(path, "openapi.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		src.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	all := src.String()
	for _, o := range operations() {
		for _, e := range o.Errors {
			for _, code := range e.Codes {
				if !strings.Contains(all, `"`+code+`"`) {
					t.Errorf("%s documents error code %q that no source produces", o.Id, code)
				}
			}
		}
	}
}

func TestOpenAPIServed(t *testing.T) {
	r := newTestRouter(t)
	w := do(r, http.MethodGet, "/api/v1/openapi.json", nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), OpenAPIDocument()) {
		t.Fatalf("openapi.json: %d", w.Code)
	}
}

// TestRateLimitedRoutesExist guards the rate-limit table against typos.
func TestRateLimitedRoutesExist(t *testing.T) {
	routes := fullRouter(t)
	for key, class := range routeLimit {
		if !slices.Contains(routes, key) {
			t.Errorf("rate-limited route %s is not registered", key)
		}
		if _, ok := DefaultRateLimits[class]; !ok {
			t.Errorf("route %s uses unknown class %s", key, class)
		}
	}
}
