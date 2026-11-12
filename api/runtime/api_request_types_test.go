package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/installation"
	"github.com/arksnorman/open-kase/api/internal/users"
)

// Owner request 2026-11-12: admin-managed request types.

func (f *ticketFixture) rtList(tok, query string) []map[string]any {
	f.t.Helper()
	return f.do("GET", "/api/v1/request-types"+query, nil, bearer(tok)).expect(200).Items()
}

// rtAll is every type (active and inactive), as an admin sees them.
func (f *ticketFixture) rtAll() []map[string]any {
	f.t.Helper()
	return f.rtList(f.admin.Access, "?active=all")
}

func (f *ticketFixture) rt(code string) map[string]any {
	f.t.Helper()
	for _, it := range f.rtAll() {
		if it["code"] == code {
			return it
		}
	}
	f.t.Fatalf("no request type with code %q", code)
	return nil
}

func (f *ticketFixture) rtID(code string) string { return f.rt(code)["id"].(string) }

func (f *ticketFixture) rtCodes(query string) string {
	f.t.Helper()
	var out []string
	for _, it := range f.rtList(f.admin.Access, query) {
		out = append(out, it["code"].(string))
	}
	return strings.Join(out, ",")
}

func (f *ticketFixture) rtCreate(name, code string) result {
	f.t.Helper()
	body := map[string]any{"name": name}
	if code != "" {
		body["code"] = code
	}
	return f.do("POST", "/api/v1/request-types", body, bearer(f.admin.Access))
}

// rtPatch changes the type with the given code at its current version.
func (f *ticketFixture) rtPatch(code string, body map[string]any) result {
	f.t.Helper()
	t := f.rt(code)
	return f.do("PATCH", "/api/v1/request-types/"+t["id"].(string), body, bearer(f.admin.Access), ifMatchV(int64(t["version"].(float64))))
}

func (f *ticketFixture) rtOrder(codes ...string) result {
	f.t.Helper()
	ids := make([]string, 0, len(codes))
	for _, c := range codes {
		ids = append(ids, f.rtID(c))
	}
	return f.do("PUT", "/api/v1/request-types/order", map[string]any{"ids": ids}, bearer(f.admin.Access))
}

func (f *ticketFixture) rtDelete(code string) result {
	f.t.Helper()
	t := f.rt(code)
	return f.do("DELETE", "/api/v1/request-types/"+t["id"].(string), nil, bearer(f.admin.Access), ifMatchV(int64(t["version"].(float64))))
}

func (f *ticketFixture) ticketType(key string) map[string]any {
	f.t.Helper()
	return obj(f.get(f.admin.Access, key).expect(200).JSON(), "requestType")
}

func (f *ticketFixture) rtAudits(action string) [][]string {
	f.t.Helper()
	evs, err := f.c.Audit.RecentAudit(context.Background(), 5000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out [][]string
	for _, ev := range evs {
		if ev.Action == action {
			out = append(out, ev.Changes)
		}
	}
	return out
}

// expectField asserts a 422 with a field error on field carrying code.
func (r result) expectField(field, code string) {
	r.t.Helper()
	r.expect(422, "validationFailed")
	for _, e := range r.Problem().Errors {
		if e.Field == field && e.Code == code {
			return
		}
	}
	r.t.Fatalf("want field error %s/%s: %s", field, code, r.Body)
}

func TestRequestTypesList(t *testing.T) {
	f := newTicketFixture(t)
	// Seeded types, in order, active only by default, with the new fields.
	items := f.rtList(f.dana.Access, "")
	if len(items) != 4 {
		t.Fatalf("seeded: %v", items)
	}
	for i, want := range []string{"issue", "question", "serviceRequest", "featureRequest"} {
		it := items[i]
		if it["code"] != want || it["active"] != true || it["order"] != float64((i+1)*10) || it["version"] != float64(1) ||
			it["id"] == "" || it["name"] == "" || it["createdAt"] == nil || it["inUse"] != float64(0) {
			t.Fatalf("item %d: %v", i, it)
		}
	}
	if items[0]["name"] != "Issue" || items[2]["name"] != "Service request" {
		t.Fatalf("names: %v", items)
	}

	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	if got := f.rtCodes(""); got != "issue,serviceRequest,featureRequest" {
		t.Fatalf("default: %s", got)
	}
	if got := f.rtCodes("?active=true"); got != "issue,serviceRequest,featureRequest" {
		t.Fatalf("active=true: %s", got)
	}
	if got := f.rtCodes("?active=false"); got != "question" {
		t.Fatalf("active=false: %s", got)
	}
	if got := f.rtCodes("?active=all"); got != "issue,question,serviceRequest,featureRequest" {
		t.Fatalf("active=all: %s", got)
	}
	f.do("GET", "/api/v1/request-types?active=maybe", nil, bearer(f.admin.Access)).expectField("active", "invalidChoice")

	// Agents, requester-side tokens and any other token read the list.
	_, create := f.token("Create only", "tickets:create")
	_, contacts := f.token("Contacts", "contacts:read")
	for _, tok := range []string{f.dana.Access, f.integration, f.readOnlyIntegraton, create, contacts} {
		if n := len(f.rtList(tok, "")); n != 3 {
			t.Fatalf("token read: %d items", n)
		}
	}
	f.do("GET", "/api/v1/request-types", nil).expect(401)

	// inUse is reported to admins and is 0 for everyone else.
	f.createTicket(f.dana.Access, map[string]any{})
	f.createTicket(f.dana.Access, map[string]any{"requestTypeId": f.rtID("serviceRequest")})
	f.createTicket(f.dana.Access, map[string]any{"requestTypeId": f.rtID("serviceRequest")})
	inUse := func(tok string) map[string]float64 {
		out := map[string]float64{}
		for _, it := range f.rtList(tok, "?active=all") {
			out[it["code"].(string)] = it["inUse"].(float64)
		}
		return out
	}
	if got := inUse(f.admin.Access); got["issue"] != 1 || got["serviceRequest"] != 2 || got["question"] != 0 {
		t.Fatalf("admin inUse: %v", got)
	}
	if got := inUse(f.dana.Access); got["issue"] != 0 || got["serviceRequest"] != 0 {
		t.Fatalf("agent inUse: %v", got)
	}
}

func TestRequestTypesCreate(t *testing.T) {
	f := newTicketFixture(t)
	// A code is derived from the name; the new type is active and last.
	got := f.rtCreate("Billing", "").expect(201).JSON()
	if got["code"] != "billing" || got["name"] != "Billing" || got["active"] != true || got["order"] != float64(50) ||
		got["version"] != float64(1) || got["inUse"] != float64(0) || got["id"] == "" {
		t.Fatalf("created: %v", got)
	}
	for _, c := range [][2]string{{"API access", "apiAccess"}, {"  Refund  request ", "refundRequest"}, {"Bug-report!", "bugReport"},
		{"3D printing", "dPrinting"}, {"Café menu", "cafMenu"}} {
		if got := f.rtCreate(c[0], "").expect(201).Str("code"); got != c[1] {
			t.Errorf("%q: code %q, want %q", c[0], got, c[1])
		}
	}
	// An explicit code.
	if got := f.rtCreate("Returns", "rma").expect(201).JSON(); got["code"] != "rma" {
		t.Fatalf("explicit: %v", got)
	}
	if codes := f.rtCodes(""); !strings.HasSuffix(codes, "billing,apiAccess,refundRequest,bugReport,dPrinting,cafMenu,rma") {
		t.Fatalf("order: %s", codes)
	}

	// Names are unique ignoring case and spaces, also among inactive types.
	f.rtCreate("  BILLING ", "other").expect(409, "requestTypeExists")
	f.rtPatch("rma", map[string]any{"active": false}).expect(200)
	f.rtCreate("returns", "rma2").expect(409, "requestTypeExists")
	// Explicit code taken (also by an inactive type); derived code taken gets a number.
	f.rtCreate("Something else", "billing").expect(409, "requestTypeCodeExists")
	f.rtCreate("Something else", "rma").expect(409, "requestTypeCodeExists")
	if c := f.rtCreate("Billing!", "").expect(201).Str("code"); c != "billing2" {
		t.Fatalf("suffix: %s", c)
	}
	if c := f.rtCreate("Billing?", "").expect(201).Str("code"); c != "billing3" {
		t.Fatalf("suffix: %s", c)
	}
	// A derived code is cut to 40 characters, also with a suffix.
	long := strings.Repeat("a", 45)
	if c := f.rtCreate(long, "").expect(201).Str("code"); c != strings.Repeat("a", 40) {
		t.Fatalf("long: %s", c)
	}
	f.rtCreate(long+" ", "").expect(409, "requestTypeExists")
	if c := f.rtCreate(long+"b", "").expect(201).Str("code"); c != strings.Repeat("a", 39)+"2" {
		t.Fatalf("long suffix: %s", c)
	}

	// Invalid names and codes.
	f.rtCreate("", "").expectField("name", "required")
	f.rtCreate(strings.Repeat("n", 61), "x1").expectField("name", "tooLong")
	for _, bad := range []string{"Bad", "1abc", "a", "has space", "a-b", strings.Repeat("a", 41)} {
		r := f.rtCreate("Valid name "+bad, bad).expect(422, "validationFailed")
		if errs := r.Problem().Errors; len(errs) != 1 || errs[0].Field != "code" {
			t.Fatalf("code %q: %s", bad, r.Body)
		}
	}
	// Names that give no valid code need an explicit one.
	f.rtCreate("Ünï", "").expectField("code", "invalidFormat")
	f.rtCreate("Q", "").expectField("code", "invalidFormat")
	f.rtCreate("123 456", "").expectField("code", "invalidFormat")
	f.rtCreate("Q", "qq").expect(201)
	f.do("POST", "/api/v1/request-types", map[string]any{"name": "x1", "active": false}, bearer(f.admin.Access)).expect(422)

	// Admins only, no token scope.
	body := map[string]any{"name": "Nope"}
	_, all := f.token("All scopes", "tickets:all", "contacts:write", "tags:write", "organisations:write", "holdReasons:write")
	for _, tok := range []string{f.dana.Access, f.integration, all} {
		f.do("POST", "/api/v1/request-types", body, bearer(tok)).expect(403)
	}
	if evs := f.rtAudits("requestType.created"); len(evs) != 12 || !slices.Equal(evs[0], []string{"name", "code"}) {
		t.Fatalf("audit: %v", evs)
	}
}

func TestRequestTypesLimit(t *testing.T) {
	f := newTicketFixture(t)
	for i := 5; i <= 50; i++ {
		f.rtCreate(fmt.Sprintf("Type %d", i), "").expect(201)
	}
	if n := len(f.rtAll()); n != 50 {
		t.Fatalf("%d types", n)
	}
	f.rtCreate("One too many", "").expect(409, "requestTypeLimitReached")
	// Inactive types count; deleting one makes room.
	f.rtPatch("type50", map[string]any{"active": false}).expect(200)
	f.rtCreate("One too many", "").expect(409, "requestTypeLimitReached")
	f.rtDelete("type50").expect(204)
	f.rtCreate("One too many", "").expect(201)
}

func TestRequestTypesUpdate(t *testing.T) {
	f := newTicketFixture(t)
	id := f.rtID("question")
	path := "/api/v1/request-types/" + id

	// Rename: version 2, same code and order; If-Match is required and checked.
	f.do("PATCH", path, map[string]any{"name": "Questions"}, bearer(f.admin.Access)).expect(428)
	f.do("PATCH", path, map[string]any{"name": "Questions"}, bearer(f.admin.Access), ifMatchV(7)).expect(409, "versionConflict")
	got := f.do("PATCH", path, map[string]any{"name": "  Questions "}, bearer(f.admin.Access), ifMatchV(1)).expect(200).JSON()
	if got["name"] != "Questions" || got["code"] != "question" || got["order"] != float64(20) || got["version"] != float64(2) || got["active"] != true {
		t.Fatalf("renamed: %v", got)
	}
	f.do("PATCH", path, map[string]any{"name": "Again"}, bearer(f.admin.Access), ifMatchV(1)).expect(409, "versionConflict")

	// Duplicate names (ignoring case, also inactive types); renaming to its own name in another case is fine.
	f.rtPatch("question", map[string]any{"name": "ISSUE"}).expect(409, "requestTypeExists")
	f.rtPatch("featureRequest", map[string]any{"active": false}).expect(200)
	f.rtPatch("question", map[string]any{"name": "feature REQUEST"}).expect(409, "requestTypeExists")
	f.rtPatch("question", map[string]any{"name": "QUESTIONS"}).expect(200)
	f.rtPatch("question", map[string]any{"name": ""}).expectField("name", "required")
	f.rtPatch("question", map[string]any{"name": strings.Repeat("n", 61)}).expectField("name", "tooLong")

	// The code cannot change.
	f.rtPatch("question", map[string]any{"code": "questions"}).expect(422)
	if f.rt("question")["code"] != "question" {
		t.Fatal("code changed")
	}

	// Deactivate and reactivate; each is a new version and an audit event.
	v := f.rt("question")["version"].(float64)
	off := f.rtPatch("question", map[string]any{"active": false}).expect(200).JSON()
	if off["active"] != false || off["version"] != v+1 {
		t.Fatalf("deactivated: %v", off)
	}
	on := f.rtPatch("question", map[string]any{"active": true}).expect(200).JSON()
	if on["active"] != true || on["version"] != v+2 {
		t.Fatalf("reactivated: %v", on)
	}

	// A no-op changes nothing: no version bump, no audit event.
	before := len(f.rtAudits("requestType.updated"))
	same := f.rtPatch("question", map[string]any{"name": "QUESTIONS", "active": true}).expect(200).JSON()
	if same["version"] != v+2 || len(f.rtAudits("requestType.updated")) != before {
		t.Fatalf("no-op: %v, audits %d -> %d", same, before, len(f.rtAudits("requestType.updated")))
	}
	f.rtPatch("question", map[string]any{}).expect(200)
	if f.rt("question")["version"] != v+2 {
		t.Fatal("empty patch bumped the version")
	}
	audits := f.rtAudits("requestType.updated")
	if !slices.Equal(audits[0], []string{"active"}) || !slices.Equal(audits[len(audits)-1], []string{"name"}) {
		t.Fatalf("audit changes: %v", audits)
	}

	// The last active type cannot be deactivated.
	for _, c := range []string{"issue", "serviceRequest"} {
		f.rtPatch(c, map[string]any{"active": false}).expect(200)
	}
	f.rtPatch("question", map[string]any{"active": false}).expect(409, "requestTypeLastActive")
	if f.rtCodes("") != "question" {
		t.Fatalf("active: %s", f.rtCodes(""))
	}

	// Unknown ids, admins only.
	f.do("PATCH", "/api/v1/request-types/000000000000000000000000", map[string]any{"name": "X"}, bearer(f.admin.Access), ifMatchV(1)).expect(404, "requestTypeNotFound")
	f.do("PATCH", "/api/v1/request-types/nope", map[string]any{"name": "X"}, bearer(f.admin.Access), ifMatchV(1)).expect(404, "requestTypeNotFound")
	f.do("PATCH", path, map[string]any{"name": "X"}, bearer(f.dana.Access), ifMatchV(1)).expect(403)
	f.do("PATCH", path, map[string]any{"name": "X"}, bearer(f.integration), ifMatchV(1)).expect(403)
}

func TestRequestTypesReorder(t *testing.T) {
	f := newTicketFixture(t)
	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	before := map[string]float64{}
	for _, it := range f.rtAll() {
		before[it["code"].(string)] = it["version"].(float64)
	}

	// Every type, active and inactive, in the new order; orders are 10, 20, ...
	got := f.rtOrder("featureRequest", "question", "issue", "serviceRequest").expect(200).Items()
	var codes []string
	for i, it := range got {
		codes = append(codes, it["code"].(string))
		if it["order"] != float64((i+1)*10) {
			t.Fatalf("orders: %v", got)
		}
	}
	if strings.Join(codes, ",") != "featureRequest,question,issue,serviceRequest" {
		t.Fatalf("returned: %v", codes)
	}
	if f.rtCodes("") != "featureRequest,issue,serviceRequest" || f.rtCodes("?active=all") != "featureRequest,question,issue,serviceRequest" {
		t.Fatalf("list: %s", f.rtCodes("?active=all"))
	}
	// Moved types get a new version; types that stayed put do not.
	after := map[string]float64{}
	for _, it := range got {
		after[it["code"].(string)] = it["version"].(float64)
	}
	if after["featureRequest"] != before["featureRequest"]+1 || after["issue"] != before["issue"]+1 || after["question"] != before["question"] ||
		after["serviceRequest"] != before["serviceRequest"]+1 {
		t.Fatalf("versions: %v -> %v", before, after)
	}
	if audits := f.rtAudits("requestType.reordered"); len(audits) != 1 || !slices.Equal(audits[0], []string{"order"}) {
		t.Fatalf("audit: %v", audits)
	}

	// The same order again writes nothing.
	f.rtOrder("featureRequest", "question", "issue", "serviceRequest").expect(200)
	if len(f.rtAudits("requestType.reordered")) != 1 {
		t.Fatal("no-op reorder audited")
	}
	if f.rt("issue")["version"] != after["issue"] {
		t.Fatal("no-op reorder bumped versions")
	}
	// Moving one type only bumps the types whose order changed.
	f.rtOrder("featureRequest", "question", "serviceRequest", "issue").expect(200)
	if f.rt("featureRequest")["version"] != after["featureRequest"] || f.rt("question")["version"] != after["question"] ||
		f.rt("issue")["version"] != after["issue"]+1 {
		t.Fatalf("partial move: %v", f.rtAll())
	}

	// A set that does not match the current types is a conflict.
	ids := func(codes ...string) []string {
		var out []string
		for _, c := range codes {
			out = append(out, f.rtID(c))
		}
		return out
	}
	put := func(ids []string) result {
		return f.do("PUT", "/api/v1/request-types/order", map[string]any{"ids": ids}, bearer(f.admin.Access))
	}
	put(ids("issue", "question", "serviceRequest")).expect(409, "requestTypeOrderConflict")
	put(append(ids("issue", "question", "serviceRequest", "featureRequest"), "000000000000000000000000")).expect(409, "requestTypeOrderConflict")
	put(append(ids("issue", "question", "serviceRequest"), "000000000000000000000000")).expect(409, "requestTypeOrderConflict")
	// Malformed, empty and repeated ids are field errors.
	put([]string{}).expectField("ids", "required")
	put(append(ids("issue", "question", "serviceRequest"), "nope")).expectField("ids", "invalidFormat")
	put(append(ids("issue", "question", "serviceRequest"), f.rtID("issue"))).expectField("ids", "duplicate")
	f.do("PUT", "/api/v1/request-types/order", map[string]any{}, bearer(f.admin.Access)).expectField("ids", "required")
	// Admins only.
	body := map[string]any{"ids": ids("issue", "question", "serviceRequest", "featureRequest")}
	f.do("PUT", "/api/v1/request-types/order", body, bearer(f.dana.Access)).expect(403)
	f.do("PUT", "/api/v1/request-types/order", body, bearer(f.integration)).expect(403)
}

func TestRequestTypesDelete(t *testing.T) {
	f := newTicketFixture(t)
	spare := f.rtCreate("Spare", "").expect(201).JSON()
	id := spare["id"].(string)

	f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.admin.Access)).expect(428)
	f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.admin.Access), ifMatchV(9)).expect(409, "versionConflict")
	f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.dana.Access), ifMatchV(1)).expect(403)
	f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.integration), ifMatchV(1)).expect(403)
	f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.admin.Access), ifMatchV(1)).expect(204)
	if got := f.rtCodes("?active=all"); strings.Contains(got, "spare") {
		t.Fatalf("still listed: %s", got)
	}
	f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.admin.Access), ifMatchV(1)).expect(404, "requestTypeNotFound")
	if audits := f.rtAudits("requestType.deleted"); len(audits) != 1 {
		t.Fatalf("audit: %v", audits)
	}
	// The code is free again.
	f.rtCreate("Spare", "").expect(201)

	// A type carried by a ticket cannot be deleted, active or not.
	f.createTicket(f.dana.Access, map[string]any{"requestTypeId": f.rtID("question")})
	f.rtDelete("question").expect(409, "requestTypeInUse")
	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	f.rtDelete("question").expect(409, "requestTypeInUse")
	if f.rt("question")["inUse"] != float64(1) || f.rt("question")["active"] != false {
		t.Fatalf("question changed: %v", f.rt("question"))
	}
	// An unused inactive type can always be deleted.
	f.rtPatch("featureRequest", map[string]any{"active": false}).expect(200)
	f.rtDelete("featureRequest").expect(204)

	// The last active type cannot be deleted.
	for _, c := range []string{"issue", "serviceRequest"} {
		f.rtPatch(c, map[string]any{"active": false}).expect(200)
	}
	f.rtDelete("spare").expect(409, "requestTypeLastActive")
	f.rtPatch("issue", map[string]any{"active": true}).expect(200)
	f.rtDelete("spare").expect(204)
	f.rtDelete("issue").expect(409, "requestTypeLastActive")
}

func TestRequestTypesOnTickets(t *testing.T) {
	f := newTicketFixture(t)
	issue, question, service := f.rtID("issue"), f.rtID("question"), f.rtID("serviceRequest")

	// The default is the first active type in order.
	a := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	if rt := f.ticketType(a); rt["id"] != issue || rt["code"] != "issue" || rt["name"] != "Issue" {
		t.Fatalf("default: %v", rt)
	}
	f.rtOrder("serviceRequest", "issue", "question", "featureRequest").expect(200)
	b := f.createTicket(f.integration, map[string]any{}).Ref()
	if rt := f.ticketType(b); rt["id"] != service || rt["code"] != "serviceRequest" || rt["name"] != "Service request" {
		t.Fatalf("after reorder: %v", rt)
	}
	f.rtPatch("serviceRequest", map[string]any{"active": false}).expect(200)
	if rt := f.ticketType(f.createTicket(f.dana.Access, map[string]any{}).Ref()); rt["code"] != "issue" {
		t.Fatalf("after deactivating the first: %v", rt)
	}
	f.rtPatch("issue", map[string]any{"active": false}).expect(200)
	if rt := f.ticketType(f.createTicket(f.dana.Access, map[string]any{}).Ref()); rt["code"] != "question" {
		t.Fatalf("after deactivating issue: %v", rt)
	}
	// Ticket b still shows the inactive type it was created with.
	if rt := f.ticketType(b); rt["code"] != "serviceRequest" || rt["name"] != "Service request" || rt["id"] != service {
		t.Fatalf("inactive type on ticket: %v", rt)
	}

	// An inactive type is refused on create (every kind of caller) and on update; an unknown one is notFound.
	for _, tok := range []string{f.dana.Access, f.integration} {
		f.do("POST", "/api/v1/tickets", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
			"requestTypeId": issue}, bearer(tok)).expectField("requestTypeId", "inactive")
	}
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
		"requestTypeId": "000000000000000000000000"}, bearer(f.dana.Access)).expectField("requestTypeId", "notFound")
	f.do("PATCH", tpath(a), map[string]any{"requestTypeId": service}, bearer(f.dana.Access), ifMatchV(f.version(a))).expectField("requestTypeId", "inactive")
	f.do("PATCH", tpath(a), map[string]any{"requestTypeId": "000000000000000000000000"}, bearer(f.dana.Access), ifMatchV(f.version(a))).expectField("requestTypeId", "notFound")
	// An active one works.
	got := f.do("PATCH", tpath(a), map[string]any{"requestTypeId": question}, bearer(f.dana.Access), ifMatchV(f.version(a))).expect(200).JSON()
	if obj(got, "requestType")["code"] != "question" {
		t.Fatalf("patched: %v", got["requestType"])
	}

	// A ticket that carries a type that became inactive is found by the filter,
	// keeps its type and can still be edited (also with its own type sent again).
	f.rtPatch("question", map[string]any{"active": true}).expect(200)
	c := f.createTicket(f.dana.Access, map[string]any{"requestTypeId": question}).Ref()
	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	f.rtPatch("featureRequest", map[string]any{"active": true}).expect(200)
	found := keys(f.do("GET", "/api/v1/tickets?requestTypeId="+question, nil, bearer(f.dana.Access)).expect(200).Items())
	if !slices.Contains(found, c) || !slices.Contains(found, a) {
		t.Fatalf("filter by inactive type: %v", found)
	}
	edited := f.do("PATCH", tpath(c), map[string]any{"priority": 1, "requestTypeId": question}, bearer(f.dana.Access), ifMatchV(f.version(c))).expect(200).JSON()
	if edited["priority"] != float64(1) || obj(edited, "requestType")["code"] != "question" || obj(edited, "requestType")["name"] != "Question" {
		t.Fatalf("edit: %v", edited)
	}

	// Follow-up and copy of a ticket with an inactive type get the default type.
	if want := f.rtCodes(""); want != "featureRequest" {
		t.Fatalf("active: %s", want)
	}
	fu := f.do("POST", tpath(c, "/follow-up"), map[string]any{}, bearer(f.dana.Access)).expect(201).Ref()
	if rt := f.ticketType(fu); rt["code"] != "featureRequest" {
		t.Fatalf("follow-up: %v", rt)
	}
	cp := f.do("POST", tpath(c, "/copy"), map[string]any{}, bearer(f.dana.Access)).expect(201).Ref()
	if rt := f.ticketType(cp); rt["code"] != "featureRequest" {
		t.Fatalf("copy: %v", rt)
	}
	// An integration (requester side) follow-up gets the default too.
	if rt := f.ticketType(f.do("POST", tpath(c, "/follow-up"), map[string]any{}, bearer(f.integration)).expect(201).Ref()); rt["code"] != "featureRequest" {
		t.Fatalf("token follow-up: %v", rt)
	}
	// Explicitly requested inactive types are refused.
	f.do("POST", tpath(c, "/follow-up"), map[string]any{"requestTypeId": question}, bearer(f.dana.Access)).expectField("requestTypeId", "inactive")
	f.do("POST", tpath(c, "/copy"), map[string]any{"requestTypeId": question}, bearer(f.dana.Access)).expectField("requestTypeId", "inactive")
	// A follow-up or copy of a ticket with an active type keeps it.
	f.rtPatch("issue", map[string]any{"active": true}).expect(200)
	d := f.createTicket(f.dana.Access, map[string]any{"requestTypeId": f.rtID("issue")}).Ref()
	f.rtOrder("featureRequest", "issue", "question", "serviceRequest").expect(200)
	for _, path := range []string{"/follow-up", "/copy"} {
		if rt := f.ticketType(f.do("POST", tpath(d, path), map[string]any{}, bearer(f.dana.Access)).expect(201).Ref()); rt["code"] != "issue" {
			t.Fatalf("%s keeps the active type: %v", path, rt)
		}
	}
	// ...and an explicit active type overrides it.
	if rt := f.ticketType(f.do("POST", tpath(d, "/copy"), map[string]any{"requestTypeId": f.rtID("featureRequest")}, bearer(f.dana.Access)).expect(201).Ref()); rt["code"] != "featureRequest" {
		t.Fatalf("override: %v", rt)
	}
}

func TestRequestTypeCodeInTicketsWebhooksAndMCP(t *testing.T) {
	nf := newNotifyFixture(t)
	nf.c.Boot.Env = EnvDevelopment // plain http to the local test receiver
	hs := newHookServer(t)
	nf.addHook(hs, "ticket.created", "ticket.updated")

	service := nf.rtID("serviceRequest")
	key := nf.createTicket(nf.dana.Access, map[string]any{"requestTypeId": service}).Ref()
	tk := nf.get(nf.dana.Access, key).expect(200).JSON()
	if rt := obj(tk, "requestType"); rt["id"] != service || rt["code"] != "serviceRequest" || rt["name"] != "Service request" || len(rt) != 3 {
		t.Fatalf("ticket: %v", rt)
	}
	// The list carries it too.
	for _, it := range nf.do("GET", "/api/v1/tickets", nil, bearer(nf.integration)).expect(200).Items() {
		if obj(it, "requestType")["code"] != "serviceRequest" {
			t.Fatalf("list: %v", it["requestType"])
		}
	}

	// A change of the request type reaches ticket.updated with id, code and name.
	nf.drain()
	question := nf.rtID("question")
	nf.do("PATCH", tpath(key), map[string]any{"requestTypeId": question}, bearer(nf.dana.Access), ifMatchV(nf.version(key))).expect(200)
	nf.drain()
	by := hookEnvelopes(t, hs)
	created, updated := obj(by["ticket.created"][0], "data"), obj(by["ticket.updated"][0], "data")
	if rt := obj(created, "requestType"); rt["id"] != service || rt["code"] != "serviceRequest" || rt["name"] != "Service request" {
		t.Fatalf("ticket.created: %v", rt)
	}
	if rt := obj(updated, "requestType"); rt["id"] != question || rt["code"] != "question" || rt["name"] != "Question" {
		t.Fatalf("ticket.updated: %v", rt)
	}
	// A rename shows in later payloads (the code stays the mapping key).
	nf.rtPatch("question", map[string]any{"name": "Ask us"}).expect(200)
	nf.do("PATCH", tpath(key), map[string]any{"priority": 1}, bearer(nf.dana.Access), ifMatchV(nf.version(key))).expect(200)
	nf.drain()
	ups := hookEnvelopes(t, hs)["ticket.updated"]
	if rt := obj(obj(ups[len(ups)-1], "data"), "requestType"); rt["code"] != "question" || rt["name"] != "Ask us" {
		t.Fatalf("ticket.updated after rename: %v", rt)
	}
}

// A ticket.updated payload built after the ticket moved on to another type
// carries only the id of the type the event is about: never that id beside
// the code and name of the ticket's current type.
func TestRequestTypeWebhookAfterTheTicketMovedOn(t *testing.T) {
	nf := newNotifyFixture(t)
	nf.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	nf.addHook(hs, "ticket.updated")
	question, service := nf.rtID("question"), nf.rtID("serviceRequest")
	key := nf.createTicket(nf.dana.Access, map[string]any{"requestTypeId": question}).Ref()
	nf.drain()
	// Both events are delivered after the second change.
	nf.do("PATCH", tpath(key), map[string]any{"priority": 2}, bearer(nf.dana.Access), ifMatchV(nf.version(key))).expect(200)
	nf.do("PATCH", tpath(key), map[string]any{"requestTypeId": service}, bearer(nf.dana.Access), ifMatchV(nf.version(key))).expect(200)
	nf.drain()
	ups := hookEnvelopes(t, hs)["ticket.updated"]
	if len(ups) != 2 {
		t.Fatalf("updates: %d", len(ups))
	}
	old, current := false, false
	for _, env := range ups {
		rt := obj(obj(env, "data"), "requestType")
		switch rt["id"] {
		case question:
			old = true
			if rt["code"] != nil || rt["name"] != nil {
				t.Fatalf("the event about the type the ticket left: %v", rt)
			}
		case service:
			current = true
			if rt["code"] != "serviceRequest" || rt["name"] != "Service request" {
				t.Fatalf("the event about the current type: %v", rt)
			}
		default:
			t.Fatalf("payload: %v", rt)
		}
	}
	if !old || !current {
		t.Fatalf("old type seen %v, current type seen %v", old, current)
	}
}

func TestRequestTypeCodeInMCP(t *testing.T) {
	f := newMCPFixture(t)
	_, tok := f.token("Back office", "tickets:read", "tickets:create", "tickets:write")
	s := f.connect(tok)
	service := f.rtID("serviceRequest")
	created := mustCall(t, s, "ticketsCreate", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
		"requestTypeId": service})
	if rt := obj(created, "requestType"); rt["id"] != service || rt["code"] != "serviceRequest" || rt["name"] != "Service request" {
		t.Fatalf("ticketsCreate: %v", created)
	}
	got := mustCall(t, s, "ticketsGet", map[string]any{"ticket": refOf(created)})
	if rt := obj(obj(got, "ticket"), "requestType"); rt["code"] != "serviceRequest" {
		t.Fatalf("ticketsGet: %v", got)
	}
	// The default is the first active type; an inactive one is refused.
	f.rtOrder("question", "issue", "serviceRequest", "featureRequest").expect(200)
	def := mustCall(t, s, "ticketsCreate", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"}})
	if rt := obj(def, "requestType"); rt["code"] != "question" {
		t.Fatalf("default: %v", def)
	}
	f.rtPatch("issue", map[string]any{"active": false}).expect(200)
	mustFail(t, s, "ticketsCreate", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
		"requestTypeId": f.rtID("issue")}, "validationFailed")
	// The only request type tool is the read-only list; management stays REST and web only.
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if strings.Contains(strings.ToLower(tl.Name), "requesttype") && tl.Name != "requestTypesList" {
			t.Fatalf("unexpected tool %s", tl.Name)
		}
	}
}

// v0.3.0 stored exactly these fields. An upgraded database must work
// without a migration.
var v030Fields = []string{"_id", "workspaceId", "projectId", "code", "name", "active", "order", "createdAt"}

func docFields(d bson.M) []string {
	var out []string
	for k := range d {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestRequestTypesUpgradeFromV030(t *testing.T) {
	f := newTicketFixture(t)
	ctx := context.Background()
	col := f.coll("requestTypes")
	sorted := func(fields ...string) []string { s := slices.Clone(fields); slices.Sort(s); return s }

	// What init writes has exactly the v0.3.0 fields.
	cur, err := col.Find(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	var seeded []bson.M
	if err := cur.All(ctx, &seeded); err != nil {
		t.Fatal(err)
	}
	if len(seeded) != 4 {
		t.Fatalf("seeded %d", len(seeded))
	}
	for _, d := range seeded {
		if !slices.Equal(docFields(d), sorted(v030Fields...)) || d["projectId"] != nil {
			t.Fatalf("seeded document: %v", d)
		}
	}

	// Replace them with fresh v0.3.0 documents written through the raw collection.
	ws := seeded[0]["workspaceId"]
	if _, err := col.DeleteMany(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	created := seeded[0]["createdAt"]
	for i, c := range []struct{ code, name string }{{"issue", "Issue"}, {"question", "Question"}, {"serviceRequest", "Service request"}, {"featureRequest", "Feature request"}} {
		if _, err := col.InsertOne(ctx, bson.D{{Key: "_id", Value: bson.NewObjectID()}, {Key: "workspaceId", Value: ws}, {Key: "projectId", Value: nil},
			{Key: "code", Value: c.code}, {Key: "name", Value: c.name}, {Key: "active", Value: true}, {Key: "order", Value: int64((i + 1) * 10)},
			{Key: "createdAt", Value: created}}); err != nil {
			t.Fatal(err)
		}
	}

	// List: versions read as 1.
	for _, it := range f.rtAll() {
		if it["version"] != float64(1) || it["active"] != true {
			t.Fatalf("list: %v", it)
		}
	}
	// Ticket creation with and without a type (touches the document).
	issue, question := f.rtID("issue"), f.rtID("question")
	if rt := f.ticketType(f.createTicket(f.dana.Access, map[string]any{}).Ref()); rt["code"] != "issue" || rt["id"] != issue {
		t.Fatalf("default: %v", rt)
	}
	f.createTicket(f.dana.Access, map[string]any{"requestTypeId": f.rtID("serviceRequest")})
	// A ticket only adds lastUsedAt to the type it uses; there is no version yet.
	var touched bson.M
	if err := col.FindOne(ctx, bson.D{{Key: "code", Value: "issue"}}).Decode(&touched); err != nil {
		t.Fatal(err)
	}
	if got := docFields(touched); !slices.Equal(got, sorted(append(slices.Clone(v030Fields), "lastUsedAt")...)) {
		t.Fatalf("issue after a ticket: %v", got)
	}
	// Create: the new document is stored like the others, plus a version.
	f.rtCreate("Billing", "").expect(201)
	var billing bson.M
	if err := col.FindOne(ctx, bson.D{{Key: "code", Value: "billing"}}).Decode(&billing); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docFields(billing), sorted(append(slices.Clone(v030Fields), "version")...)) || billing["projectId"] != nil {
		t.Fatalf("new document: %v", billing)
	}
	// Rename with If-Match: 1, deactivate, reorder, delete.
	f.do("PATCH", "/api/v1/request-types/"+question, map[string]any{"name": "Questions"}, bearer(f.admin.Access), ifMatchV(1)).expect(200)
	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	f.rtOrder("billing", "issue", "question", "serviceRequest", "featureRequest").expect(200)
	f.rtDelete("featureRequest").expect(204)
	f.rtPatch("question", map[string]any{"active": true}).expect(200)
	if rt := f.ticketType(f.createTicket(f.dana.Access, map[string]any{}).Ref()); rt["code"] != "billing" {
		t.Fatalf("default after reorder: %v", rt)
	}
	f.createTicket(f.dana.Access, map[string]any{"requestTypeId": question})

	// A document nothing touched is still exactly the v0.3.0 shape; touched
	// ones gain only version and lastUsedAt.
	var untouched bson.M
	if err := col.FindOne(ctx, bson.D{{Key: "code", Value: "issue"}}).Decode(&untouched); err != nil {
		t.Fatal(err)
	}
	if got := docFields(untouched); !slices.Equal(got, sorted(append(slices.Clone(v030Fields), "lastUsedAt", "version")...)) {
		t.Fatalf("issue document: %v", got)
	}
	var serviceDoc bson.M
	if err := col.FindOne(ctx, bson.D{{Key: "code", Value: "serviceRequest"}}).Decode(&serviceDoc); err != nil {
		t.Fatal(err)
	}
	if got := docFields(serviceDoc); !slices.Equal(got, sorted(append(slices.Clone(v030Fields), "lastUsedAt", "version")...)) {
		t.Fatalf("serviceRequest document: %v", got)
	}
	if n, _ := col.CountDocuments(ctx, bson.D{}); n != 4 {
		t.Fatalf("documents: %d", n)
	}
}

// Without a ticket or a reorder, an upgraded document stays exactly as v0.3.0
// wrote it, whatever else is managed.
func TestRequestTypesUntouchedDocumentsStayAsTheyWere(t *testing.T) {
	f := newTicketFixture(t)
	ctx := context.Background()
	col := f.coll("requestTypes")
	f.rtCreate("Billing", "").expect(201)
	f.rtPatch("billing", map[string]any{"name": "Billing and refunds"}).expect(200)
	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	var d bson.M
	if err := col.FindOne(ctx, bson.D{{Key: "code", Value: "serviceRequest"}}).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(docFields(d), func() []string { s := slices.Clone(v030Fields); slices.Sort(s); return s }()) {
		t.Fatalf("untouched document: %v", d)
	}
}

func TestInitDoesNotRecreateDeletedRequestTypes(t *testing.T) {
	f := newTicketFixture(t)
	ctx := context.Background()
	in := installation.Input{WorkspaceName: "Acme", AdminEmail: adminEmail, AdminName: users.PersonName{Given: "Ada", Family: "Admin"}, AdminPassword: adminPassword}

	f.rtDelete("featureRequest").expect(204)
	f.rtPatch("question", map[string]any{"name": "Ask us"}).expect(200)
	res, err := f.c.Installation.Initialize(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestTypesCreated != 0 {
		t.Fatalf("init created %d request types", res.RequestTypesCreated)
	}
	if got := f.rtCodes("?active=all"); got != "issue,question,serviceRequest" {
		t.Fatalf("after init: %s", got)
	}
	if f.rt("question")["name"] != "Ask us" {
		t.Fatal("init renamed a type back")
	}
	// Admin-created types survive as well, and a second init changes nothing.
	f.rtCreate("Billing", "").expect(201)
	if _, err := f.c.Installation.Initialize(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := f.rtCodes("?active=all"); got != "issue,question,serviceRequest,billing" {
		t.Fatalf("after second init: %s", got)
	}
}

// A ticket write and a concurrent delete or deactivation of its request type
// never leave a ticket on a deleted type, and never answer 500.
func TestRequestTypesRaceWithTicketCreate(t *testing.T) {
	f := newTicketFixture(t)
	const writers = 4
	for round := range 6 {
		rt := f.rtCreate(fmt.Sprintf("Race %d", round), "").expect(201).JSON()
		id := rt["id"].(string)
		deleting := round%2 == 0
		creates := make([]result, writers)
		var change result
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range writers {
			wg.Go(func() {
				<-start
				creates[i] = f.do("POST", "/api/v1/tickets", map[string]any{"subject": "Race", "body": "Body.", "requestTypeId": id,
					"requester": map[string]any{"name": "Racer", "email": fmt.Sprintf("racer%d-%d@acme.example", round, i)}}, bearer(f.dana.Access))
			})
		}
		wg.Go(func() {
			<-start
			if deleting {
				change = f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.admin.Access), ifMatchV(1))
			} else {
				change = f.do("PATCH", "/api/v1/request-types/"+id, map[string]any{"active": false}, bearer(f.admin.Access), ifMatchV(1))
			}
		})
		close(start)
		wg.Wait()

		created := 0
		for _, r := range creates {
			switch {
			case r.Code == 201:
				created++
			case r.Code == 422 && strings.Contains(string(r.Body), `"requestTypeId"`):
			default:
				t.Fatalf("round %d: create answered %d %s", round, r.Code, r.Body)
			}
		}
		stored, err := f.coll("tickets").CountDocuments(context.Background(), bson.D{{Key: "requestTypeId", Value: mustObjectID(t, id)}})
		if err != nil {
			t.Fatal(err)
		}
		if int(stored) != created {
			t.Fatalf("round %d: %d tickets stored, %d created", round, stored, created)
		}
		switch {
		case deleting && change.Code == 204:
			if created != 0 {
				t.Fatalf("round %d: deleted a type that %d tickets carry", round, created)
			}
		case deleting && change.Code == 409 && change.Problem().Code == "requestTypeInUse":
			if created == 0 {
				t.Fatalf("round %d: requestTypeInUse without a ticket", round)
			}
		case !deleting && change.Code == 200:
		default:
			t.Fatalf("round %d: change answered %d %s", round, change.Code, change.Body)
		}
	}
}

// concurrently runs the requests at the same moment and returns their answers in order.
func concurrently(fns ...func() result) []result {
	out := make([]result, len(fns))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, fn := range fns {
		wg.Go(func() {
			<-start
			out[i] = fn()
		})
	}
	close(start)
	wg.Wait()
	return out
}

func answers(rs []result) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "%d %s; ", r.Code, r.Body)
	}
	return b.String()
}

// Two admins deactivate the last two active types at the same moment: one
// wins, the other is refused, and a type stays active.
func TestRequestTypesConcurrentDeactivationKeepsOneActive(t *testing.T) {
	f := newTicketFixture(t)
	f.rtPatch("serviceRequest", map[string]any{"active": false}).expect(200)
	f.rtPatch("featureRequest", map[string]any{"active": false}).expect(200)
	for round := range 5 {
		for _, c := range []string{"issue", "question"} {
			if f.rt(c)["active"] != true {
				f.rtPatch(c, map[string]any{"active": true}).expect(200)
			}
		}
		off := func(tok string, rt map[string]any) func() result {
			return func() result {
				return f.do("PATCH", "/api/v1/request-types/"+rt["id"].(string), map[string]any{"active": false}, bearer(tok), ifMatchV(int64(rt["version"].(float64))))
			}
		}
		rs := concurrently(off(f.admin.Access, f.rt("issue")), off(f.lead.Access, f.rt("question")))
		if n := len(f.rtList(f.admin.Access, "")); n != 1 {
			t.Fatalf("round %d: %d active types left: %s", round, n, answers(rs))
		}
		won := 0
		for _, r := range rs {
			switch {
			case r.Code == 200:
				won++
			case r.Code == 409 && r.Problem().Code == "requestTypeLastActive":
			default:
				t.Fatalf("round %d: %s", round, answers(rs))
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d deactivations succeeded: %s", round, won, answers(rs))
		}
	}
}

// Two admins create types with the same name (and different codes) at the
// same moment: one is created, the other is refused.
func TestRequestTypesConcurrentCreateSameName(t *testing.T) {
	f := newTicketFixture(t)
	for round := range 5 {
		name := fmt.Sprintf("Twin %d", round)
		add := func(tok, code string) func() result {
			return func() result {
				return f.do("POST", "/api/v1/request-types", map[string]any{"name": name, "code": code}, bearer(tok))
			}
		}
		rs := concurrently(add(f.admin.Access, fmt.Sprintf("twinA%d", round)), add(f.lead.Access, fmt.Sprintf("twinB%d", round)))
		created := 0
		for _, r := range rs {
			switch {
			case r.Code == 201:
				created++
			case r.Code == 409 && r.Problem().Code == "requestTypeExists":
			default:
				t.Fatalf("round %d: %s", round, answers(rs))
			}
		}
		if created != 1 {
			t.Fatalf("round %d: %d created: %s", round, created, answers(rs))
		}
	}
}

// A ticket update that sets a type and a concurrent delete of that type
// never leave the ticket on a deleted type, and never answer 500.
func TestRequestTypesRaceWithTicketUpdate(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	for round := range 8 {
		id := f.rtCreate(fmt.Sprintf("Update race %d", round), "").expect(201).JSON()["id"].(string)
		v := f.version(key)
		rs := concurrently(
			func() result {
				return f.do("PATCH", tpath(key), map[string]any{"requestTypeId": id}, bearer(f.dana.Access), ifMatchV(v))
			},
			func() result {
				return f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.admin.Access), ifMatchV(1))
			},
		)
		upd, del := rs[0], rs[1]
		stored, err := f.coll("tickets").CountDocuments(context.Background(), bson.D{{Key: "requestTypeId", Value: mustObjectID(t, id)}})
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case upd.Code == 200 && del.Code == 409 && del.Problem().Code == "requestTypeInUse" && stored == 1:
			// The update won; the next round moves the ticket to a new type.
		case upd.Code == 422 && strings.Contains(string(upd.Body), `"requestTypeId"`) && del.Code == 204 && stored == 0:
			// The delete won, and the ticket keeps the type it had.
			if got := f.ticketType(key)["code"]; got == nil || got == "" {
				t.Fatalf("round %d: ticket type after a refused update: %v", round, f.ticketType(key))
			}
		default:
			t.Fatalf("round %d: update %d %s, delete %d %s, %d stored", round, upd.Code, upd.Body, del.Code, del.Body, stored)
		}
	}
}

// A ticket create that names an organisation and a concurrent deactivation
// or delete of that organisation answer a field error, never 500, and never
// leave a ticket on a deleted organisation.
func TestOrganisationRaceWithTicketCreate(t *testing.T) {
	f := newTicketFixture(t)
	const writers = 4
	for round := range 6 {
		id := f.do("POST", "/api/v1/organisations", map[string]any{"name": fmt.Sprintf("Race org %d", round)}, bearer(f.admin.Access)).expect(201).JSON()["id"].(string)
		deleting := round%2 == 0
		fns := make([]func() result, 0, writers+1)
		for i := range writers {
			fns = append(fns, func() result {
				return f.do("POST", "/api/v1/tickets", map[string]any{"subject": "Race", "body": "Body.", "organisationId": id,
					"requester": map[string]any{"name": "Racer", "email": fmt.Sprintf("orgracer%d-%d@acme.example", round, i)}}, bearer(f.dana.Access))
			})
		}
		fns = append(fns, func() result {
			if deleting {
				return f.do("DELETE", "/api/v1/organisations/"+id, nil, bearer(f.admin.Access), ifMatchV(1))
			}
			return f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"active": false}, bearer(f.admin.Access), ifMatchV(1))
		})
		rs := concurrently(fns...)
		creates, change := rs[:writers], rs[writers]

		created := 0
		for _, r := range creates {
			switch {
			case r.Code == 201:
				created++
			case r.Code == 422 && strings.Contains(string(r.Body), `"organisationId"`):
			default:
				t.Fatalf("round %d: create answered %d %s", round, r.Code, r.Body)
			}
		}
		stored, err := f.coll("tickets").CountDocuments(context.Background(), bson.D{{Key: "organisationId", Value: mustObjectID(t, id)}})
		if err != nil {
			t.Fatal(err)
		}
		if int(stored) != created {
			t.Fatalf("round %d: %d tickets stored, %d created", round, stored, created)
		}
		switch {
		case deleting && change.Code == 204:
			if created != 0 {
				t.Fatalf("round %d: deleted an organisation that %d tickets carry", round, created)
			}
		case deleting && change.Code == 409 && change.Problem().Code == "organisationInUse":
			if created == 0 {
				t.Fatalf("round %d: organisationInUse without a ticket", round)
			}
		case !deleting && change.Code == 200:
		default:
			t.Fatalf("round %d: change answered %d %s", round, change.Code, change.Body)
		}
	}
}

// Owner request 2026-11-12: requestTypeCode wherever requestTypeId is accepted.

func newTicketBody(extra map[string]any) map[string]any {
	b := map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func TestRequestTypeCodeOnCreate(t *testing.T) {
	f := newTicketFixture(t)
	service := f.rtID("serviceRequest")
	// A user and a requester-side token may both name the type by code.
	for _, tok := range []string{f.dana.Access, f.integration} {
		key := f.do("POST", "/api/v1/tickets", newTicketBody(map[string]any{"requestTypeCode": "serviceRequest"}), bearer(tok)).expect(201).Ref()
		if rt := f.ticketType(key); rt["id"] != service || rt["code"] != "serviceRequest" {
			t.Fatalf("by code: %v", rt)
		}
	}
	post := func(extra map[string]any) result {
		return f.do("POST", "/api/v1/tickets", newTicketBody(extra), bearer(f.dana.Access))
	}
	// Both fields: refused even when they name the same type.
	post(map[string]any{"requestTypeId": service, "requestTypeCode": "serviceRequest"}).expectField("requestTypeCode", "notAllowed")
	post(map[string]any{"requestTypeId": f.rtID("issue"), "requestTypeCode": "serviceRequest"}).expectField("requestTypeCode", "notAllowed")
	post(map[string]any{"requestTypeCode": "nosuchtype"}).expectField("requestTypeCode", "notFound")
	post(map[string]any{"requestTypeCode": "servicerequest"}).expectField("requestTypeCode", "notFound") // case-sensitive
	post(map[string]any{"requestTypeCode": "Bad code"}).expectField("requestTypeCode", "invalidFormat")
	post(map[string]any{"requestTypeCode": ""}).expect(422, "validationFailed")
	f.rtPatch("serviceRequest", map[string]any{"active": false}).expect(200)
	post(map[string]any{"requestTypeCode": "serviceRequest"}).expectField("requestTypeCode", "inactive")
	f.do("POST", "/api/v1/tickets", newTicketBody(map[string]any{"requestTypeCode": "serviceRequest"}), bearer(f.integration)).expectField("requestTypeCode", "inactive")
}

func TestRequestTypeCodeOnUpdate(t *testing.T) {
	nf := newNotifyFixture(t)
	nf.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	nf.addHook(hs, "ticket.updated")
	key := nf.createTicket(nf.dana.Access, map[string]any{"requestTypeCode": "issue"}).Ref()
	nf.drain()
	patch := func(tok string, body map[string]any) result {
		return nf.do("PATCH", tpath(key), body, bearer(tok), ifMatchV(nf.version(key)))
	}
	before := len(nf.rtAudits("ticket.updated"))
	got := patch(nf.dana.Access, map[string]any{"requestTypeCode": "question"}).expect(200).JSON()
	if rt := obj(got, "requestType"); rt["id"] != nf.rtID("question") || rt["code"] != "question" {
		t.Fatalf("by code: %v", rt)
	}
	audits := nf.rtAudits("ticket.updated")
	if len(audits) != before+1 || !slices.Equal(audits[len(audits)-1], []string{"requestType"}) {
		t.Fatalf("audit: %v", audits)
	}
	nf.drain()
	ups := hookEnvelopes(t, hs)["ticket.updated"]
	if len(ups) != 1 || obj(obj(ups[0], "data"), "requestType")["code"] != "question" {
		t.Fatalf("ticket.updated: %v", ups)
	}

	// The same type is a no-op: same version, no audit event, also when it is inactive.
	v := nf.version(key)
	patch(nf.dana.Access, map[string]any{"requestTypeCode": "question"}).expect(200)
	nf.rtPatch("question", map[string]any{"active": false}).expect(200)
	patch(nf.dana.Access, map[string]any{"requestTypeCode": "question"}).expect(200)
	if nf.version(key) != v || len(nf.rtAudits("ticket.updated")) != before+1 {
		t.Fatalf("no-op changed the ticket: version %d -> %d, audits %d", v, nf.version(key), len(nf.rtAudits("ticket.updated")))
	}
	// Refusals.
	patch(nf.dana.Access, map[string]any{"requestTypeCode": "issue", "requestTypeId": nf.rtID("issue")}).expectField("requestTypeCode", "notAllowed")
	patch(nf.dana.Access, map[string]any{"requestTypeCode": "nosuchtype"}).expectField("requestTypeCode", "notFound")
	patch(nf.dana.Access, map[string]any{"requestTypeCode": "Bad code"}).expectField("requestTypeCode", "invalidFormat")
	patch(nf.dana.Access, map[string]any{"requestTypeCode": "serviceRequest"}).expect(200)
	nf.rtPatch("featureRequest", map[string]any{"active": false}).expect(200)
	patch(nf.dana.Access, map[string]any{"requestTypeCode": "featureRequest"}).expectField("requestTypeCode", "inactive")

	// A requester-side token is refused exactly as it is for requestTypeId.
	ikey := nf.createTicket(nf.integration, map[string]any{}).Ref()
	byID := nf.do("PATCH", tpath(ikey), map[string]any{"requestTypeId": nf.rtID("issue")}, bearer(nf.integration)).Code
	byCode := nf.do("PATCH", tpath(ikey), map[string]any{"requestTypeCode": "issue"}, bearer(nf.integration))
	if byID != 403 || byCode.Code != byID {
		t.Fatalf("requester-side update: by id %d, by code %d %s", byID, byCode.Code, byCode.Body)
	}
}

func TestRequestTypeCodeOnFollowUpAndCopy(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{"requestTypeCode": "issue"}).Ref()
	for _, path := range []string{"/follow-up", "/copy"} {
		got := f.do("POST", tpath(key, path), map[string]any{"requestTypeCode": "question"}, bearer(f.dana.Access)).expect(201).Ref()
		if rt := f.ticketType(got); rt["code"] != "question" || rt["id"] != f.rtID("question") {
			t.Fatalf("%s: %v", path, rt)
		}
		f.do("POST", tpath(key, path), map[string]any{"requestTypeCode": "question", "requestTypeId": f.rtID("question")}, bearer(f.dana.Access)).expectField("requestTypeCode", "notAllowed")
		f.do("POST", tpath(key, path), map[string]any{"requestTypeCode": "nosuchtype"}, bearer(f.dana.Access)).expectField("requestTypeCode", "notFound")
		f.do("POST", tpath(key, path), map[string]any{"requestTypeCode": "Bad code"}, bearer(f.dana.Access)).expectField("requestTypeCode", "invalidFormat")
	}
	// A requester-side follow-up with a code is refused like one with an ID.
	byID := f.do("POST", tpath(key, "/follow-up"), map[string]any{"requestTypeId": f.rtID("question")}, bearer(f.integration))
	byCode := f.do("POST", tpath(key, "/follow-up"), map[string]any{"requestTypeCode": "question"}, bearer(f.integration))
	if byID.Code != 403 || byCode.Code != 403 || byID.Problem().Code != byCode.Problem().Code {
		t.Fatalf("requester-side follow-up: by id %d %s, by code %d %s", byID.Code, byID.Body, byCode.Code, byCode.Body)
	}
	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	f.do("POST", tpath(key, "/follow-up"), map[string]any{"requestTypeCode": "question"}, bearer(f.dana.Access)).expectField("requestTypeCode", "inactive")
	f.do("POST", tpath(key, "/copy"), map[string]any{"requestTypeCode": "question"}, bearer(f.dana.Access)).expectField("requestTypeCode", "inactive")
}

func TestRequestTypeCodeListFilter(t *testing.T) {
	f := newTicketFixture(t)
	a := f.createTicket(f.dana.Access, map[string]any{"requestTypeCode": "issue"}).Ref()
	b := f.createTicket(f.dana.Access, map[string]any{"requestTypeCode": "question"}).Ref()
	c := f.createTicket(f.dana.Access, map[string]any{"requestTypeCode": "serviceRequest"}).Ref()
	list := func(q string) []string {
		return keys(f.do("GET", "/api/v1/tickets?"+q, nil, bearer(f.dana.Access)).expect(200).Items())
	}
	if got := list("requestTypeCode=issue"); !slices.Equal(got, []string{a}) {
		t.Fatalf("one code: %v", got)
	}
	got := list("requestTypeCode=issue,question")
	slices.Sort(got)
	if want := []string{a, b}; !slices.Equal(got, want) {
		t.Fatalf("two codes: %v want %v", got, want)
	}
	// An unknown code among known ones is ignored; all unknown is an empty page.
	if got := list("requestTypeCode=issue,nosuchtype"); !slices.Equal(got, []string{a}) {
		t.Fatalf("unknown among known: %v", got)
	}
	if got := list("requestTypeCode=nosuchtype"); len(got) != 0 {
		t.Fatalf("unknown code: %v", got)
	}
	// An inactive type's code still finds its tickets.
	f.rtPatch("serviceRequest", map[string]any{"active": false}).expect(200)
	if got := list("requestTypeCode=serviceRequest"); !slices.Equal(got, []string{c}) {
		t.Fatalf("inactive type: %v", got)
	}
	// Refusals.
	f.do("GET", "/api/v1/tickets?requestTypeCode=issue&requestTypeId="+f.rtID("issue"), nil, bearer(f.dana.Access)).expectField("requestTypeCode", "notAllowed")
	f.do("GET", "/api/v1/tickets?requestTypeCode=Bad%20code", nil, bearer(f.dana.Access)).expectField("requestTypeCodes[0]", "invalidFormat")
	f.do("GET", "/api/v1/tickets?requestTypeCode=issue,Bad", nil, bearer(f.dana.Access)).expectField("requestTypeCodes[1]", "invalidFormat")
	many := strings.TrimSuffix(strings.Repeat("issue,", 21), ",")
	f.do("GET", "/api/v1/tickets?requestTypeCode="+many, nil, bearer(f.dana.Access)).expectField("requestTypeCodes", "tooLong")
	byID := f.do("GET", "/api/v1/tickets?requestTypeId="+f.rtID("issue"), nil, bearer(f.integration))
	byCode := f.do("GET", "/api/v1/tickets?requestTypeCode=issue", nil, bearer(f.integration))
	if byID.Code != 403 || byCode.Code != 403 || byID.Problem().Code != byCode.Problem().Code {
		t.Fatalf("requester-side filter: by id %d %s, by code %d %s", byID.Code, byID.Body, byCode.Code, byCode.Body)
	}
}

func TestRequestTypeCodeRaceWithTicketUpdateAndCreate(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	for round := range 8 {
		code := fmt.Sprintf("raceType%d", round)
		id := f.rtCreate(fmt.Sprintf("Code race %d", round), code).expect(201).JSON()["id"].(string)
		v := f.version(key)
		rs := concurrently(
			func() result {
				return f.do("PATCH", tpath(key), map[string]any{"requestTypeCode": code}, bearer(f.dana.Access), ifMatchV(v))
			},
			func() result {
				return f.do("POST", "/api/v1/tickets", newTicketBody(map[string]any{"requestTypeCode": code}), bearer(f.dana.Access))
			},
			func() result {
				return f.do("DELETE", "/api/v1/request-types/"+id, nil, bearer(f.admin.Access), ifMatchV(1))
			},
		)
		for _, r := range rs[:2] {
			switch r.Code {
			case 200, 201:
			case 422:
				if strings.Contains(string(r.Body), `"requestTypeId"`) || !strings.Contains(string(r.Body), `"requestTypeCode"`) {
					t.Fatalf("round %d: the refusal must name requestTypeCode: %s", round, r.Body)
				}
			default:
				t.Fatalf("round %d: %d %s", round, r.Code, r.Body)
			}
		}
		if rs[2].Code != 204 && rs[2].Code != 409 {
			t.Fatalf("round %d: delete %d %s", round, rs[2].Code, rs[2].Body)
		}
	}
}

func TestMCPRequestTypesList(t *testing.T) {
	f := newMCPFixture(t)
	_, req := f.integration("Portal", "tickets:create", "tickets:replyAsRequester")
	for name, tok := range map[string]string{"user": f.dana.Access, "requester-side token": req} {
		s := f.connect(tok)
		items := mustCall(t, s, "requestTypesList", nil)["items"].([]any)
		if len(items) != 4 {
			t.Fatalf("%s default: %v", name, items)
		}
		first := items[0].(map[string]any)
		if first["code"] != "issue" || first["id"] == "" || first["name"] != "Issue" || first["active"] != true {
			t.Fatalf("%s item: %v", name, first)
		}
		mustFail(t, s, "requestTypesList", map[string]any{"active": "maybe"}, "validationFailed")
	}
	f.rtPatch("question", map[string]any{"active": false}).expect(200)
	s := f.connect(f.dana.Access)
	codes := func(args map[string]any) string {
		var out []string
		for _, it := range mustCall(t, s, "requestTypesList", args)["items"].([]any) {
			out = append(out, it.(map[string]any)["code"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := codes(nil); got != f.rtCodes("") || strings.Contains(got, "question") {
		t.Fatalf("default: %s", got)
	}
	if got := codes(map[string]any{"active": "true"}); strings.Contains(got, "question") {
		t.Fatalf("active=true: %s", got)
	}
	if got := codes(map[string]any{"active": "false"}); got != "question" {
		t.Fatalf("active=false: %s", got)
	}
	if got := codes(map[string]any{"active": "all"}); got != f.rtCodes("?active=all") || !strings.Contains(got, "question") {
		t.Fatalf("active=all: %s", got)
	}
}

func TestMCPTicketsWithRequestTypeCode(t *testing.T) {
	f := newMCPFixture(t)
	_, tok := f.token("Back office", "tickets:read", "tickets:create", "tickets:write")
	s := f.connect(tok)
	created := mustCall(t, s, "ticketsCreate", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
		"requestTypeCode": "serviceRequest"})
	if rt := obj(created, "requestType"); rt["id"] != f.rtID("serviceRequest") || rt["code"] != "serviceRequest" {
		t.Fatalf("ticketsCreate: %v", created)
	}
	mustFail(t, s, "ticketsCreate", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
		"requestTypeCode": "serviceRequest", "requestTypeId": f.rtID("serviceRequest")}, "validationFailed")
	mustFail(t, s, "ticketsCreate", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
		"requestTypeCode": "nosuchtype"}, "validationFailed")

	upd := mustCall(t, s, "ticketsUpdate", map[string]any{"ticket": refOf(created), "requestTypeCode": "question"})
	if rt := obj(upd, "requestType"); rt["code"] != "question" {
		t.Fatalf("ticketsUpdate: %v", upd)
	}
	other := mustCall(t, s, "ticketsCreate", map[string]any{"subject": "S", "body": "B", "requester": map[string]any{"name": "P", "email": "p@acme.example"},
		"requestTypeCode": "issue"})
	for _, c := range []struct{ filter, want string }{{"question", refOf(created)}, {"issue", refOf(other)}} {
		items := mustCall(t, s, "ticketsList", map[string]any{"requestTypeCode": c.filter})["items"].([]any)
		if len(items) != 1 || refOf(items[0].(map[string]any)) != c.want {
			t.Fatalf("ticketsList %s: %v", c.filter, items)
		}
	}
	if items := mustCall(t, s, "ticketsList", map[string]any{"requestTypeCode": "nosuchtype"})["items"].([]any); len(items) != 0 {
		t.Fatalf("unknown code: %v", items)
	}
	mustFail(t, s, "ticketsList", map[string]any{"requestTypeCode": "issue", "requestTypeId": f.rtID("issue")}, "validationFailed")
}
