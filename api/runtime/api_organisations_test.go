package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Owner decision 2026-10-31: organisations (customers of an integrating
// system) set explicitly by the integration; never inferred.

func (f *ticketFixture) createOrg(tok, name, ref string) map[string]any {
	f.t.Helper()
	body := map[string]any{"name": name}
	if ref != "" {
		body["externalRef"] = ref
	}
	return f.do("POST", "/api/v1/organisations", body, bearer(tok)).expect(201).JSON()
}

func TestOrganisationsCRUD(t *testing.T) {
	f := newMCPFixture(t)
	_, reader := f.token("Org reader", "organisations:read")
	_, writer := f.token("Org writer", "organisations:write")
	_, stafft := f.token("Staff bot", "tickets:read", "tickets:write")
	_, all := f.token("All bot", "tickets:all")
	_, other := f.token("Create only", "tickets:create")

	// Scopes: staff users, readers, writers and staff tokens read; managing
	// needs an admin or organisations:write (tickets:all does not include it).
	for _, tok := range []string{f.dana.Access, reader, writer, stafft, all} {
		f.do("GET", "/api/v1/organisations", nil, bearer(tok)).expect(200)
	}
	f.do("GET", "/api/v1/organisations", nil, bearer(other)).expect(403, "insufficientScope")
	body := map[string]any{"name": "Acme Ltd", "externalRef": "CUST-0042"}
	for _, tok := range []string{f.dana.Access, reader, stafft, all, other} {
		f.do("POST", "/api/v1/organisations", body, bearer(tok)).expect(403)
	}
	acme := f.do("POST", "/api/v1/organisations", body, bearer(writer)).expect(201).JSON()
	if acme["name"] != "Acme Ltd" || acme["externalRef"] != "CUST-0042" || acme["version"] != float64(1) || acme["contactCount"] != float64(0) {
		t.Fatalf("created %v", acme)
	}
	id := acme["id"].(string)
	adminOrg := f.createOrg(f.admin.Access, "Northwind Traders", "")
	if adminOrg["externalRef"] != nil {
		t.Fatalf("no ref: %v", adminOrg)
	}

	// Uniqueness: name (case-insensitive) and external ref.
	f.do("POST", "/api/v1/organisations", map[string]any{"name": "ACME LTD"}, bearer(f.admin.Access)).expect(409, "organisationExists")
	f.do("POST", "/api/v1/organisations", map[string]any{"name": "Other", "externalRef": "CUST-0042"}, bearer(f.admin.Access)).expect(409, "organisationRefTaken")
	// Several organisations without an external ref are fine.
	f.createOrg(f.admin.Access, "No ref one", "")
	f.createOrg(f.admin.Access, "No ref two", "")
	for _, bad := range []map[string]any{{"name": ""}, {}, {"name": strings.Repeat("x", 201)}, {"name": "x", "externalRef": strings.Repeat("r", 129)}, {"name": "x", "bogus": 1}} {
		f.do("POST", "/api/v1/organisations", bad, bearer(f.admin.Access)).expect(422)
	}

	// Counts reach managers only; staff see zero.
	got := f.do("GET", "/api/v1/organisations/"+id, nil, bearer(f.dana.Access)).expect(200).JSON()
	if got["name"] != "Acme Ltd" {
		t.Fatalf("get %v", got)
	}
	f.do("GET", "/api/v1/organisations/000000000000000000000000", nil, bearer(f.dana.Access)).expect(404, "organisationNotFound")

	// Search by name or external ref, paged.
	byName := f.do("GET", "/api/v1/organisations?q=acm", nil, bearer(f.dana.Access)).expect(200).Items()
	byRef := f.do("GET", "/api/v1/organisations?q=cust-00", nil, bearer(f.dana.Access)).expect(200).Items()
	if len(byName) != 1 || len(byRef) != 1 || byRef[0]["id"] != id {
		t.Fatalf("search: %v %v", byName, byRef)
	}
	pg := f.do("GET", "/api/v1/organisations?limit=2", nil, bearer(f.admin.Access)).expect(200).JSON()
	if len(pg["items"].([]any)) != 2 || pg["nextCursor"] == nil {
		t.Fatalf("page: %v", pg)
	}

	// Update needs If-Match; admins and organisations:write only.
	f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"name": "Acme Limited"}, bearer(writer)).expect(428)
	f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"name": "Acme Limited"}, bearer(f.dana.Access), ifMatchV(1)).expect(403)
	up := f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"name": "Acme Limited", "externalRef": ""}, bearer(writer), ifMatchV(1)).expect(200).JSON()
	if up["name"] != "Acme Limited" || up["externalRef"] != nil || up["version"] != float64(2) {
		t.Fatalf("updated %v", up)
	}
	f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"name": "Again"}, bearer(writer), ifMatchV(1)).expect(409, "versionConflict")
	f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"name": "Northwind Traders"}, bearer(writer), ifMatchV(2)).expect(409, "organisationExists")

	// Delete is refused while a contact or ticket references the organisation.
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": id}, bearer(f.admin.Access)).expect(201)
	f.do("DELETE", "/api/v1/organisations/"+id, nil, bearer(writer), ifMatchV(2)).expect(409, "organisationInUse")
	list := f.do("GET", "/api/v1/organisations?q=acme", nil, bearer(f.admin.Access)).expect(200).Items()
	if list[0]["contactCount"] != float64(1) || list[0]["ticketCount"] != float64(0) {
		t.Fatalf("counts %v", list[0])
	}
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": ""}, bearer(f.admin.Access)).expect(200)
	tk := f.createTicket(f.ticketFixture.integration, map[string]any{"organisationId": id}).JSON()
	f.do("DELETE", "/api/v1/organisations/"+id, nil, bearer(writer), ifMatchV(2)).expect(409, "organisationInUse")
	// Get and Update report real counts to managers, 0 to read-only callers.
	if g := f.do("GET", "/api/v1/organisations/"+id, nil, bearer(f.admin.Access)).expect(200).JSON(); g["ticketCount"].(float64) < 1 {
		t.Fatalf("get counts %v", g)
	}
	if g := f.do("GET", "/api/v1/organisations/"+id, nil, bearer(reader)).expect(200).JSON(); g["ticketCount"] != float64(0) || g["contactCount"] != float64(0) {
		t.Fatalf("reader counts %v", g)
	}
	if u := f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"externalRef": "CUST-0042"}, bearer(f.admin.Access), ifMatchV(2)).expect(200).JSON(); u["ticketCount"].(float64) < 1 {
		t.Fatalf("update counts %v", u)
	}
	// The ticket came from an integration, so only an integration may clear its organisation.
	_, tw := f.integration("Ticket writer", "tickets:read", "tickets:write")
	f.do("PATCH", tpath(refOf(tk)), map[string]any{"organisationId": ""}, bearer(tw), ifMatchV(f.version(refOf(tk)))).expect(200)
	f.do("DELETE", "/api/v1/organisations/"+id, nil, bearer(f.dana.Access), ifMatchV(3)).expect(403)
	f.do("DELETE", "/api/v1/organisations/"+id, nil, bearer(writer), ifMatchV(3)).expect(204)
	f.do("DELETE", "/api/v1/organisations/"+id, nil, bearer(writer), ifMatchV(3)).expect(404, "organisationNotFound")

	// Every change is audited.
	evs, err := f.c.Audit.RecentAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.Action] = true
	}
	for _, a := range []string{"organisation.created", "organisation.updated", "organisation.deleted", "contact.updated"} {
		if !seen[a] {
			t.Errorf("missing audit %s", a)
		}
	}
}

func TestOrganisationOnContactsAndTickets(t *testing.T) {
	f := newTicketFixture(t)
	acme := f.createOrg(f.admin.Access, "Acme Ltd", "CUST-1")["id"].(string)
	globex := f.createOrg(f.admin.Access, "Globex", "")["id"].(string)
	_, contactsW := f.token("Contacts", "contacts:write", "contacts:read")

	// Contacts: set by the integration, validated, changeable, clearable.
	c := f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": acme}, bearer(contactsW)).expect(201).JSON()
	if c["organisationId"] != acme {
		t.Fatalf("contact %v", c)
	}
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "X", "email": "x@acme.example", "organisationId": "000000000000000000000000"}, bearer(contactsW)).expect(422, "validationFailed")
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "X", "email": "x@acme.example", "organisationId": "nope"}, bearer(contactsW)).expect(422)
	// Omitted: unchanged.
	same := f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example"}, bearer(contactsW)).expect(200).JSON()
	if same["organisationId"] != acme {
		t.Fatalf("omitted organisationId changed the link: %v", same)
	}
	moved := f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": globex}, bearer(contactsW)).expect(200).JSON()
	if moved["organisationId"] != globex {
		t.Fatalf("moved %v", moved)
	}
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": ""}, bearer(contactsW)).expect(200)
	if got := f.do("GET", "/api/v1/contacts/"+c["id"].(string), nil, bearer(contactsW)).expect(200).JSON(); got["organisationId"] != nil {
		t.Fatalf("cleared %v", got)
	}

	// Tickets: the organisation is explicit; a contact's organisation is never inferred.
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": acme}, bearer(contactsW)).expect(200)
	plain := f.createTicket(f.integration, map[string]any{}).JSON()
	if plain["organisation"] != nil {
		t.Fatalf("organisation inferred from the contact: %v", plain["organisation"])
	}
	tk := f.createTicket(f.integration, map[string]any{"organisationId": acme}).JSON()
	org := obj(tk, "organisation")
	if org["id"] != acme || org["name"] != "Acme Ltd" || org["externalRef"] != "CUST-1" {
		t.Fatalf("created: %v", tk)
	}
	ref := refOf(tk)
	if got := obj(f.get(f.integration, ref).expect(200).JSON(), "organisation"); got["name"] != "Acme Ltd" {
		t.Fatalf("requester view: %v", got)
	}
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "P", "email": "p@example.com"},
		"organisationId": "000000000000000000000000"}, bearer(f.dana.Access)).expect(422, "validationFailed")
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "P", "email": "p@example.com"},
		"orgId": "free-text"}, bearer(f.dana.Access)).expect(422)

	// Staff change and clear it; a requester-side token may not.
	_, writer := f.token("Org writer", "tickets:write", "tickets:read")
	up := f.do("PATCH", tpath(ref), map[string]any{"organisationId": globex}, bearer(writer), ifMatchV(f.version(ref))).expect(200).JSON()
	if obj(up, "organisation")["name"] != "Globex" || obj(up, "organisation")["externalRef"] != nil {
		t.Fatalf("patched: %v", up)
	}
	f.do("PATCH", tpath(ref), map[string]any{"organisationId": acme}, bearer(f.integration)).expect(403)
	f.do("PATCH", tpath(ref), map[string]any{"organisationId": "000000000000000000000000"}, bearer(writer), ifMatchV(f.version(ref))).expect(422)

	// Filter by organisation (staff).
	f.do("PATCH", tpath(ref), map[string]any{"organisationId": acme}, bearer(writer), ifMatchV(f.version(ref))).expect(200)
	items := f.do("GET", "/api/v1/tickets?organisationId="+acme, nil, bearer(f.dana.Access)).expect(200).Items()
	if len(items) != 1 || refOf(items[0]) != ref {
		t.Fatalf("filter: %v", items)
	}
	if n := len(f.do("GET", "/api/v1/tickets?organisationId="+globex, nil, bearer(f.dana.Access)).expect(200).Items()); n != 0 {
		t.Fatalf("filter globex: %d", n)
	}
	f.do("GET", "/api/v1/tickets?organisationId=bad", nil, bearer(f.dana.Access)).expect(422)

	// A new requester created with the ticket can carry an explicit organisation.
	nt := f.createTicket(f.admin.Access, map[string]any{"requester": map[string]any{"name": "Lena", "email": "lena@acme.example", "organisationId": acme}}).JSON()
	lena := f.do("GET", "/api/v1/contacts?q=lena@", nil, bearer(f.admin.Access)).expect(200).Items()
	if len(lena) != 1 || lena[0]["organisationId"] != acme || nt["organisation"] != nil {
		t.Fatalf("requester org: %v / %v", lena, nt["organisation"])
	}

	// Copy and follow-up keep the organisation reference.
	cp := f.do("POST", tpath(ref, "/copy"), map[string]any{}, bearer(f.dana.Access)).expect(201).JSON()
	if obj(cp, "organisation")["id"] != acme {
		t.Fatalf("copy: %v", cp)
	}
}

func TestOrganisationInWebhookPayload(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	f.do("POST", "/api/v1/webhooks", map[string]any{"name": "Org hook", "url": hs.URL + "/hook",
		"eventTypes": []string{"ticket.created", "ticket.updated"}}, bearer(f.admin.Access)).expect(201)
	org := f.createOrg(f.admin.Access, "Acme Ltd", "CUST-0042")["id"].(string)
	f.createTicket(f.integration, map[string]any{"organisationId": org})
	f.createTicket(f.integration, map[string]any{"subject": "No org"})
	f.drain()
	found := 0
	for _, r := range hs.all() {
		var env struct {
			Type string `json:"type"`
			Data struct {
				Subject      string          `json:"subject"`
				Organisation json.RawMessage `json:"organisation"`
			} `json:"data"`
		}
		jsonUnmarshal(t, r.body, &env)
		if env.Type != "ticket.created" {
			continue
		}
		found++
		if env.Data.Subject == "No org" {
			if string(env.Data.Organisation) != "null" {
				t.Fatalf("no organisation must be null: %s", r.body)
			}
			continue
		}
		var o map[string]any
		jsonUnmarshal(t, env.Data.Organisation, &o)
		if o["id"] != org || o["name"] != "Acme Ltd" || o["externalRef"] != "CUST-0042" {
			t.Fatalf("organisation payload: %s", r.body)
		}
		if strings.Contains(string(r.body), "orgId") || strings.Contains(string(r.body), "orgName") {
			t.Fatalf("legacy org fields in payload: %s", r.body)
		}
	}
	if found != 2 {
		t.Fatalf("ticket.created deliveries: %d", found)
	}
}

func TestOrganisationsMCP(t *testing.T) {
	f := newMCPFixture(t)
	_, writer := f.integration("Org writer", "organisations:write", "contacts:write", "tickets:create")
	_, reader := f.integration("Org reader", "organisations:read")
	_, nothing := f.integration("Create only", "tickets:create")
	w, r, n := f.connect(writer), f.connect(reader), f.connect(nothing)

	created := mustCall(t, w, "organisationsCreate", map[string]any{"name": "Acme Ltd", "externalRef": "CUST-0042"})
	id := created["id"].(string)
	if created["externalRef"] != "CUST-0042" {
		t.Fatalf("created %v", created)
	}
	if res, _ := callTool(t, w, "organisationsCreate", map[string]any{"name": "acme ltd"}); res.Problem == nil || res.Problem.Code != "organisationExists" {
		t.Fatalf("duplicate: %+v", res)
	}
	if res, _ := callTool(t, r, "organisationsCreate", map[string]any{"name": "Nope"}); !res.IsError {
		t.Fatal("reader created an organisation")
	}
	if res, _ := callTool(t, n, "organisationsList", nil); !res.IsError {
		t.Fatal("a token without organisation scopes listed organisations")
	}
	if got := mustCall(t, r, "organisationsGet", map[string]any{"organisationId": id}); got["name"] != "Acme Ltd" {
		t.Fatalf("get %v", got)
	}
	if l := mustCall(t, r, "organisationsList", map[string]any{"q": "cust-0042"}); len(l["items"].([]any)) != 1 {
		t.Fatalf("list %v", l)
	}
	up := mustCall(t, w, "organisationsUpdate", map[string]any{"organisationId": id, "name": "Acme Limited", "externalRef": ""})
	if up["name"] != "Acme Limited" || up["externalRef"] != nil {
		t.Fatalf("update %v", up)
	}

	// Link a contact and a ticket; delete is refused until they are unlinked.
	ct := mustCall(t, w, "contactsUpsert", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": id})
	if obj(ct, "contact")["organisationId"] != id {
		t.Fatalf("contact %v", ct)
	}
	tk := mustCall(t, w, "ticketsCreate", map[string]any{"subject": "Help", "body": "please", "organisationId": id,
		"requester": map[string]any{"contactId": obj(ct, "contact")["id"]}})
	if obj(tk, "organisation")["id"] != id {
		t.Fatalf("ticket %v", tk)
	}
	if res, _ := callTool(t, w, "organisationsDelete", map[string]any{"organisationId": id}); res.Problem == nil || res.Problem.Code != "organisationInUse" {
		t.Fatalf("delete in use: %+v", res)
	}
	mustCall(t, w, "contactsUpsert", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": ""})
	// The ticket came from an integration, so only an integration may clear its organisation.
	_, tw := f.integration("Ticket writer", "tickets:read", "tickets:write")
	f.do("PATCH", tpath(refOf(tk)), map[string]any{"organisationId": ""}, bearer(tw), ifMatchV(f.version(refOf(tk)))).expect(200)
	if got := mustCall(t, w, "organisationsDelete", map[string]any{"organisationId": id}); got["deleted"] != id {
		t.Fatalf("delete %v", got)
	}
}

func TestRequesterOrganisationNeverSilentlyDropped(t *testing.T) {
	f := newTicketFixture(t)
	acme := f.createOrg(f.admin.Access, "Acme Ltd", "")["id"].(string)
	globex := f.createOrg(f.admin.Access, "Globex", "")["id"].(string)
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": acme}, bearer(f.admin.Access)).expect(201)
	req := func(org string) map[string]any {
		return map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": org}}
	}
	// The requester-side token cannot change an existing contact's organisation: refused, not ignored.
	f.do("POST", "/api/v1/tickets", req(globex), bearer(f.integration)).expect(403, "insufficientScope")
	f.do("POST", "/api/v1/tickets", req(""), bearer(f.integration)).expect(403, "insufficientScope")
	// An unknown organisation is a 422 even when the contact exists.
	f.do("POST", "/api/v1/tickets", req("000000000000000000000000"), bearer(f.integration)).expect(422)
	// The organisation the contact already has is fine.
	f.do("POST", "/api/v1/tickets", req(acme), bearer(f.integration)).expect(201)
	// A new contact can be created with an organisation, and an unknown one is refused.
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "New", "email": "new@x.example", "organisationId": acme}}, bearer(f.integration)).expect(201)
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "New2", "email": "new2@x.example", "organisationId": "000000000000000000000000"}}, bearer(f.integration)).expect(422)
}

func TestOrganisationNamesCollapseWhitespace(t *testing.T) {
	f := newTicketFixture(t)
	o := f.createOrg(f.admin.Access, "  Acme   Trading \t Ltd ", "")
	if o["name"] != "Acme Trading Ltd" {
		t.Fatalf("stored name %q", o["name"])
	}
	f.do("POST", "/api/v1/organisations", map[string]any{"name": "acme trading   ltd"}, bearer(f.admin.Access)).expect(409, "organisationExists")
	f.do("PATCH", "/api/v1/organisations/"+o["id"].(string), map[string]any{"name": "Acme  Trading Ltd"}, bearer(f.admin.Access), ifMatchV(1)).expect(200)
}

func TestOrganisationDeactivateAndReactivate(t *testing.T) {
	f := newTicketFixture(t)
	_, writer := f.token("Org writer", "organisations:write", "contacts:write")
	org := f.createOrg(f.admin.Access, "Acme Ltd", "CUST-1")
	id := org["id"].(string)
	if org["active"] != true {
		t.Fatalf("new organisation: %v", org)
	}
	// An existing ticket and contact reference it.
	tk := f.createTicket(f.integration, map[string]any{"organisationId": id}).JSON()
	ref := refOf(tk)
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "organisationId": id}, bearer(f.admin.Access)).expect(200)

	// Deactivating needs the manage permission and is audited as an update.
	f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"active": false}, bearer(f.dana.Access), ifMatchV(1)).expect(403)
	off := f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"active": false}, bearer(writer), ifMatchV(1)).expect(200).JSON()
	if off["active"] != false || off["version"] != float64(2) {
		t.Fatalf("deactivated: %v", off)
	}
	// Hidden from the default list; visible with active=false or all.
	if n := len(f.do("GET", "/api/v1/organisations", nil, bearer(f.dana.Access)).expect(200).Items()); n != 0 {
		t.Fatalf("default list shows %d", n)
	}
	if n := len(f.do("GET", "/api/v1/organisations?active=false", nil, bearer(f.dana.Access)).expect(200).Items()); n != 1 {
		t.Fatalf("active=false shows %d", n)
	}
	if n := len(f.do("GET", "/api/v1/organisations?active=all", nil, bearer(f.admin.Access)).expect(200).Items()); n != 1 {
		t.Fatalf("active=all shows %d", n)
	}
	f.do("GET", "/api/v1/organisations?active=maybe", nil, bearer(f.admin.Access)).expect(422)
	// Blocked for new tickets, ticket PATCH and contacts.
	body := map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "P", "email": "p@x.example"}, "organisationId": id}
	f.do("POST", "/api/v1/tickets", body, bearer(f.dana.Access)).expect(422, "validationFailed")
	other := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.do("PATCH", tpath(other), map[string]any{"organisationId": id}, bearer(f.dana.Access), ifMatchV(f.version(other))).expect(422)
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "New", "email": "new@acme.example", "organisationId": id}, bearer(f.admin.Access)).expect(422)
	// Existing tickets and contacts keep the name; the contact keeps its link when upserted again; copies keep it.
	if got := obj(f.get(f.dana.Access, ref).expect(200).JSON(), "organisation"); got["name"] != "Acme Ltd" {
		t.Fatalf("existing ticket lost its organisation: %v", got)
	}
	f.do("POST", "/api/v1/contacts", map[string]any{"name": "Priya R.", "email": "priya@acme.example", "organisationId": id}, bearer(f.admin.Access)).expect(200)
	if cp := f.do("POST", tpath(ref, "/copy"), map[string]any{}, bearer(f.dana.Access)).expect(201).JSON(); obj(cp, "organisation")["id"] != id {
		t.Fatalf("copy lost the organisation: %v", cp)
	}
	// Delete is still refused while referenced.
	f.do("DELETE", "/api/v1/organisations/"+id, nil, bearer(writer), ifMatchV(2)).expect(409, "organisationInUse")

	// Reactivate: back in the default list and linkable again.
	on := f.do("PATCH", "/api/v1/organisations/"+id, map[string]any{"active": true}, bearer(writer), ifMatchV(2)).expect(200).JSON()
	if on["active"] != true {
		t.Fatalf("reactivated: %v", on)
	}
	if n := len(f.do("GET", "/api/v1/organisations", nil, bearer(f.dana.Access)).expect(200).Items()); n != 1 {
		t.Fatalf("default list after reactivation shows %d", n)
	}
	f.do("POST", "/api/v1/tickets", body, bearer(f.dana.Access)).expect(201)
}

func TestOrganisationLockedOnIntegrationTickets(t *testing.T) {
	f := newTicketFixture(t)
	acme := f.createOrg(f.admin.Access, "Acme Ltd", "")["id"].(string)
	globex := f.createOrg(f.admin.Access, "Globex", "")["id"].(string)
	_, writer := f.token("Org writer", "tickets:write", "tickets:read")
	ref := refOf(f.createTicket(f.integration, map[string]any{"organisationId": acme}).JSON())
	patch := func(tok, org string) result {
		return f.do("PATCH", tpath(ref), map[string]any{"organisationId": org}, bearer(tok), ifMatchV(f.version(ref)))
	}
	patch(f.admin.Access, globex).expect(403, "organisationManagedByIntegration")
	patch(f.admin.Access, acme).expect(200)
	patch(writer, globex).expect(200)
	web := refOf(f.createTicket(f.admin.Access, map[string]any{}).JSON())
	f.do("PATCH", tpath(web), map[string]any{"organisationId": acme}, bearer(f.admin.Access), ifMatchV(f.version(web))).expect(200)
}
