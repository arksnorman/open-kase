package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func groupByName(items []map[string]any, name string) map[string]any {
	for _, g := range items {
		if g["name"] == name {
			return g
		}
	}
	return nil
}

func TestInitSeedsProjectAndProtectedTriage(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	p := h.project(admin)
	if _, hasKey := p.JSON()["key"]; hasKey || p.Str("name") == "" || p.Str("defaultGroupId") == "" || p.Version() != 1 {
		t.Fatalf("project: %s", p.Body)
	}
	// There is one project: no project collection routes.
	h.do("GET", "/api/v1/projects", nil, bearer(admin.Access)).expect(404, "routeNotFound")
	h.do("POST", "/api/v1/projects", map[string]any{"key": "WEB", "name": "Web"}, bearer(admin.Access)).expect(404, "routeNotFound")

	groups := h.do("GET", "/api/v1/groups", nil, bearer(admin.Access)).expect(200)
	if groups.JSON()["nextCursor"] != nil {
		t.Fatal("unexpected next cursor")
	}
	triage := groupByName(groups.Items(), "Triage")
	if triage == nil || triage["protected"] != true || triage["active"] != true || triage["id"] != p.Str("defaultGroupId") {
		t.Fatalf("triage: %s", groups.Body)
	}
	tid := triage["id"].(string)
	tv := int64(triage["version"].(float64))

	// Triage cannot be removed or deactivated, but can be renamed/described.
	h.do("DELETE", "/api/v1/groups/"+tid, nil, bearer(admin.Access)).expect(409, "groupProtected")
	h.do("PATCH", "/api/v1/groups/"+tid, map[string]any{"active": false}, bearer(admin.Access), ifMatchV(tv)).expect(409, "groupProtected")
	h.do("PATCH", "/api/v1/groups/"+tid, map[string]any{"description": "Start here"}, bearer(admin.Access), ifMatchV(tv)).expect(200)

	// Other groups can be deactivated and deleted.
	g := h.do("POST", "/api/v1/groups", map[string]any{"name": "Billing"}, bearer(admin.Access)).expect(201)
	h.do("POST", "/api/v1/groups", map[string]any{"name": "billing "}, bearer(admin.Access)).expect(409, "groupNameTaken")
	d := h.do("PATCH", "/api/v1/groups/"+g.Str("id"), map[string]any{"active": false}, bearer(admin.Access), ifMatchV(g.Version())).expect(200)
	if d.JSON()["active"] != false || d.Version() != 2 {
		t.Fatalf("deactivate: %s", d.Body)
	}
	h.do("DELETE", "/api/v1/groups/"+g.Str("id"), nil, bearer(admin.Access)).expect(204)
	h.do("GET", "/api/v1/groups/"+g.Str("id"), nil, bearer(admin.Access)).expect(404, "groupNotFound")

	// The project has no editable name, description or key any more.
	h.do("PATCH", "/api/v1/project", map[string]any{"key": "POST"}, bearer(admin.Access), ifMatchV(1)).expect(422, "validationFailed")
	h.do("PATCH", "/api/v1/project", map[string]any{"name": "Mail v2"}, bearer(admin.Access), ifMatchV(1)).expect(422, "validationFailed")
	h.do("PATCH", "/api/v1/project", map[string]any{"requesterAcknowledgement": false}, bearer(admin.Access)).expect(428)
	h.do("PATCH", "/api/v1/project", map[string]any{"status": "inactive"}, bearer(admin.Access), ifMatchV(1)).expect(422, "validationFailed")
	ack := h.do("PATCH", "/api/v1/project", map[string]any{"requesterAcknowledgement": false}, bearer(admin.Access), ifMatchV(1)).expect(200)
	h.do("PATCH", "/api/v1/project", map[string]any{"requesterAcknowledgement": true}, bearer(admin.Access), ifMatchV(1)).expect(409, "versionConflict")
	if ack.JSON()["requesterAcknowledgement"] != false || ack.Version() != 2 {
		t.Fatalf("project: %s", ack.Body)
	}

	acts := h.auditActions()
	for _, want := range []string{"project.created:success", "group.created:success", "group.deactivated:success", "group.deleted:success", "project.updated:success"} {
		if !contains(acts, want) {
			t.Fatalf("missing audit %s in %v", want, acts)
		}
	}
}

func TestRolesAndGroupMembership(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	uid := h.createUser(admin, "agent@example.com", "Agent")
	agent := h.login("agent@example.com", "agent password 1")

	// Every user is an agent of the project. Groups are a staff directory:
	// even without memberships an agent lists every group (to transfer to).
	h.do("GET", "/api/v1/project", nil, bearer(agent.Access)).expect(200)
	if gs := h.do("GET", "/api/v1/groups", nil, bearer(agent.Access)).expect(200).Items(); len(gs) != 1 {
		t.Fatalf("agent without groups sees %v, want the Triage group", gs)
	}
	// Removed multi-project routes.
	h.do("GET", "/api/v1/me/projects", nil, bearer(agent.Access)).expect(404, "routeNotFound")
	h.do("POST", "/api/v1/members", map[string]any{"userId": uid, "role": "agent"}, bearer(admin.Access)).expect(405)
	h.do("GET", "/api/v1/members/"+uid, nil, bearer(admin.Access)).expect(404, "routeNotFound")

	// The member directory lists every user with their role and groups.
	dir := h.do("GET", "/api/v1/members", nil, bearer(agent.Access)).expect(200).Items()
	roles := map[string]string{}
	for _, m := range dir {
		roles[m["user"].(map[string]any)["email"].(string)] = m["role"].(string)
	}
	if len(dir) != 2 || roles[adminEmail] != "admin" || roles["agent@example.com"] != "agent" {
		t.Fatalf("directory: %v", dir)
	}

	groups := h.do("GET", "/api/v1/groups", nil, bearer(admin.Access)).Items()
	triage := groupByName(groups, "Triage")["id"].(string)
	billing := h.do("POST", "/api/v1/groups", map[string]any{"name": "Billing"}, bearer(admin.Access)).expect(201).Str("id")
	gm := h.do("PUT", "/api/v1/groups/"+triage+"/members/"+uid, nil, bearer(admin.Access)).expect(200)
	if gm.JSON()["user"].(map[string]any)["email"] != "agent@example.com" {
		t.Fatalf("group member: %s", gm.Body)
	}
	h.do("PUT", "/api/v1/groups/"+triage+"/members/"+uid, nil, bearer(admin.Access)).expect(200) // idempotent
	// The directory searches by name, email or username, and narrows to a group or a user.
	count := func(q string) int {
		return len(h.do("GET", "/api/v1/members?"+q, nil, bearer(agent.Access)).expect(200).Items())
	}
	if n := count("q=AGENT@"); n != 1 {
		t.Fatalf("q search found %d", n)
	}
	if n := count("q=nobody-here"); n != 0 {
		t.Fatalf("q search found %d", n)
	}
	if n := count("q=.*"); n != 0 { // never a pattern
		t.Fatalf("q is a pattern: %d", n)
	}
	if n := count("groupId=" + triage); n != 1 {
		t.Fatalf("group filter found %d", n)
	}
	if n := count("userId=" + uid + "," + uid + "&active=true"); n != 1 {
		t.Fatalf("user filter found %d", n)
	}
	// Only existing users can join a group.
	h.do("PUT", "/api/v1/groups/"+triage+"/members/0123456789abcdef01234567", nil, bearer(admin.Access)).expect(422, "validationFailed")

	// Agents read the whole group directory (to transfer tickets) but cannot manage anything.
	agentGroups := h.do("GET", "/api/v1/groups", nil, bearer(agent.Access)).expect(200).Items()
	if len(agentGroups) != 2 || groupByName(agentGroups, "Billing") == nil {
		t.Fatalf("agent groups: %v", agentGroups)
	}
	h.do("GET", "/api/v1/groups/"+billing, nil, bearer(agent.Access)).expect(200)
	h.do("POST", "/api/v1/groups", map[string]any{"name": "Mine"}, bearer(agent.Access)).expect(403, "forbidden")
	h.do("PATCH", "/api/v1/project", map[string]any{"requesterAcknowledgement": false}, bearer(agent.Access), ifMatchV(1)).expect(403, "forbidden")
	h.do("GET", "/api/v1/integrations", nil, bearer(agent.Access)).expect(403)
	h.do("GET", "/api/v1/reports/summary", nil, bearer(agent.Access)).expect(403)
	h.do("GET", "/api/v1/users", nil, bearer(agent.Access)).expect(403)

	// Promotion to admin applies on the next request with the same token;
	// demotion revokes it again (and ends the user's sessions).
	h.makeAdmin(admin, uid)
	h.do("GET", "/api/v1/integrations", nil, bearer(agent.Access)).expect(200)
	if gs := h.do("GET", "/api/v1/groups", nil, bearer(agent.Access)).expect(200).Items(); len(gs) != 2 {
		t.Fatalf("admin groups: %v", gs)
	}
	v := h.do("GET", "/api/v1/users/"+uid, nil, bearer(admin.Access)).expect(200).Version()
	h.do("PATCH", "/api/v1/users/"+uid, map[string]any{"isAdmin": false}, bearer(admin.Access), ifMatchV(v)).expect(200)
	agent = h.login("agent@example.com", "agent password 1")
	h.do("GET", "/api/v1/integrations", nil, bearer(agent.Access)).expect(403)

	// Removing the group membership takes effect immediately (the group stays in the directory).
	h.do("DELETE", "/api/v1/groups/"+triage+"/members/"+uid, nil, bearer(admin.Access)).expect(204)
	if n := h.do("GET", "/api/v1/groups/"+triage, nil, bearer(agent.Access)).expect(200).JSON()["memberCount"]; n != float64(0) {
		t.Fatalf("memberCount after removal: %v", n)
	}
	if !contains(h.auditActions(), "groupMembership.removed:success") {
		t.Fatal("group membership removal not audited")
	}
}

func TestIntegrationTokenScopesAndRevocation(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.project(admin)

	scopes := h.do("GET", "/api/v1/integration-scopes", nil, bearer(admin.Access)).expect(200).Items()
	// One list for every token, each scope described once.
	var names []string
	for _, s := range scopes {
		names = append(names, s["name"].(string))
		if s["description"] == "" || s["type"] != nil {
			t.Fatalf("scope entry: %v", s)
		}
		if staff := s["staff"] == true; staff != slices.Contains([]string{"tickets:comment", "tickets:internal", "tickets:write", "tickets:all"}, s["name"].(string)) {
			t.Fatalf("scope staff flag: %v", s)
		}
	}
	if !slices.Equal(names, []string{"tickets:read", "tickets:create", "tickets:replyAsRequester", "tickets:comment", "tickets:internal",
		"tickets:write", "tickets:all", "contacts:read", "contacts:write", "tags:read", "tags:write", "holdReasons:read", "holdReasons:write", "organisations:read", "organisations:write"}) {
		t.Fatalf("scopes: %v", scopes)
	}
	// The token type is gone: sending it is an unknown field; scopes must
	// be known and at least one.
	h.do("POST", "/api/v1/integrations", map[string]any{"name": "CRM", "type": "agent", "scopes": []string{"contacts:read"}}, bearer(admin.Access)).expect(422, "validationFailed")
	h.do("POST", "/api/v1/integrations", map[string]any{"name": "CRM", "scopes": []string{}}, bearer(admin.Access)).expect(422, "validationFailed")
	h.do("POST", "/api/v1/integrations", map[string]any{"name": "CRM", "scopes": []string{"admin:all"}}, bearer(admin.Access)).expect(422)
	created := h.do("POST", "/api/v1/integrations", map[string]any{"name": "CRM", "scopes": []string{"contacts:read"}},
		bearer(admin.Access), header("Idempotency-Key", "crm-1")).expect(201)
	token := created.Str("token")
	if !strings.HasPrefix(token, "kase_live_") || created.Str("id") == "" || created.JSON()["revokedAt"] != nil {
		t.Fatalf("created: %s", created.Body)
	}
	// Retrying the create never shows the token again.
	h.do("POST", "/api/v1/integrations", map[string]any{"name": "CRM", "scopes": []string{"contacts:read"}},
		bearer(admin.Access), header("Idempotency-Key", "crm-1")).expect(409, "idempotencyKeyUsed")

	// Only the SHA-256 hash is stored.
	sum := sha256.Sum256([]byte(token))
	db := h.rawDB()
	if n, _ := db.Collection("apiCredentials").CountDocuments(context.Background(), bson.D{{Key: "tokenHash", Value: hex.EncodeToString(sum[:])}}); n != 1 {
		t.Fatal("token hash not stored")
	}
	for _, col := range []string{"apiCredentials", "servicePrincipals", "auditEvents", "idempotencyRecords"} {
		cur, _ := db.Collection(col).Find(context.Background(), bson.D{})
		var docs []bson.M
		cur.All(context.Background(), &docs)
		for _, d := range docs {
			raw, _ := bson.Marshal(d)
			if strings.Contains(string(raw), token) {
				t.Fatalf("plaintext token stored in %s", col)
			}
		}
	}
	list := h.do("GET", "/api/v1/integrations", nil, bearer(admin.Access)).expect(200).Items()
	if len(list) != 1 || list[0]["token"] != nil {
		t.Fatalf("list: %v", list)
	}

	// Scope enforcement.
	h.do("GET", "/api/v1/contacts", nil, bearer(token)).expect(200)
	h.do("GET", "/api/v1/project", nil, bearer(token)).expect(200)
	h.do("POST", "/api/v1/contacts", map[string]any{"name": "C", "email": "c@example.com"}, bearer(token)).expect(403, "insufficientScope")
	// Integrations cannot reach admin routes.
	h.do("GET", "/api/v1/integrations", nil, bearer(token)).expect(403)
	h.do("POST", "/api/v1/integrations", map[string]any{"name": "Escalate", "scopes": []string{"contacts:write"}}, bearer(token)).expect(403)
	h.do("GET", "/api/v1/users", nil, bearer(token)).expect(403)
	h.do("GET", "/api/v1/me", nil, bearer(token)).expect(403, "userRequired")
	h.do("GET", "/api/v1/groups", nil, bearer(token)).expect(403)

	// A write-scoped integration may upsert contacts.
	rw := h.do("POST", "/api/v1/integrations", map[string]any{"name": "Portal", "scopes": []string{"contacts:read", "contacts:write"}}, bearer(admin.Access)).expect(201)
	h.do("POST", "/api/v1/contacts", map[string]any{"name": "C", "email": "c@example.com"}, bearer(rw.Str("token"))).expect(201)

	// Revocation takes effect on the next request.
	h.do("DELETE", "/api/v1/integrations/"+created.Str("id"), nil, bearer(admin.Access)).expect(204)
	h.do("GET", "/api/v1/contacts", nil, bearer(token)).expect(401, "invalidToken")
	h.do("DELETE", "/api/v1/integrations/"+created.Str("id"), nil, bearer(admin.Access)).expect(204)
	h.do("GET", "/api/v1/contacts", nil, bearer(rw.Str("token"))).expect(200)
	h.do("GET", "/api/v1/contacts", nil, bearer("kase_live_forged")).expect(401, "invalidToken")

	acts := h.auditActions()
	if !contains(acts, "integration.created:success") || !contains(acts, "integration.revoked:success") {
		t.Fatalf("integration audit: %v", acts)
	}
}

func TestContactUpsert(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.project(admin)

	c1 := h.do("POST", "/api/v1/contacts", map[string]any{"name": "Cat", "email": "Cat@Example.com"}, bearer(admin.Access)).expect(201)
	if c1.JSON()["externalId"] != nil || c1.Version() != 1 {
		t.Fatalf("contact: %s", c1.Body)
	}
	// Same email (case-insensitive): update, not duplicate.
	c2 := h.do("POST", "/api/v1/contacts", map[string]any{"name": "Cat Smith", "email": "cat@example.com",
		"externalId": "crm-42"}, bearer(admin.Access)).expect(200)
	if c2.Str("id") != c1.Str("id") || c2.Str("name") != "Cat Smith" || c2.Version() != 2 || c2.Str("externalId") != "crm-42" {
		t.Fatalf("upsert by email: %s", c2.Body)
	}
	// Match by external ID, changing the email.
	c3 := h.do("POST", "/api/v1/contacts", map[string]any{"name": "Cat Smith", "email": "cat.smith@example.com", "externalId": "crm-42"}, bearer(admin.Access)).expect(200)
	if c3.Str("id") != c1.Str("id") || c3.Str("email") != "cat.smith@example.com" {
		t.Fatalf("upsert by external id: %s", c3.Body)
	}
	// Unchanged upsert keeps the version.
	c4 := h.do("POST", "/api/v1/contacts", map[string]any{"name": "Cat Smith", "email": "cat.smith@example.com"}, bearer(admin.Access)).expect(200)
	if c4.Version() != c3.Version() {
		t.Fatal("no-op upsert changed the version")
	}
	// External ID and email that identify different contacts conflict.
	h.do("POST", "/api/v1/contacts", map[string]any{"name": "Dog", "email": "dog@example.com"}, bearer(admin.Access)).expect(201)
	h.do("POST", "/api/v1/contacts", map[string]any{"name": "X", "email": "dog@example.com", "externalId": "crm-42"}, bearer(admin.Access)).expect(409, "contactConflict")
	h.do("POST", "/api/v1/contacts", map[string]any{"name": "X", "email": "x@example.com", "organisationId": "0123456789abcdef01234567"}, bearer(admin.Access)).expect(422, "validationFailed")
	h.do("POST", "/api/v1/contacts", map[string]any{"name": "X", "email": "not-an-email"}, bearer(admin.Access)).expect(422)

	list := h.do("GET", "/api/v1/contacts?limit=1", nil, bearer(admin.Access)).expect(200)
	next, _ := list.JSON()["nextCursor"].(string)
	if len(list.Items()) != 1 || next == "" {
		t.Fatalf("paging: %s", list.Body)
	}
	page2 := h.do("GET", "/api/v1/contacts?limit=1&cursor="+next, nil, bearer(admin.Access)).expect(200)
	if len(page2.Items()) != 1 || page2.Items()[0]["id"] == list.Items()[0]["id"] {
		t.Fatalf("page 2: %s", page2.Body)
	}
	h.do("GET", "/api/v1/contacts?cursor=garbage", nil, bearer(admin.Access)).expect(422)
	if items := h.do("GET", "/api/v1/contacts?q=DOG", nil, bearer(admin.Access)).expect(200).Items(); len(items) != 1 {
		t.Fatalf("contact search: %v", items)
	}
	acts := h.auditActions()
	if !contains(acts, "contact.created:success") || !contains(acts, "contact.updated:success") {
		t.Fatalf("customer audit: %v", acts)
	}
}

func TestIdempotentCreate(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	body := map[string]any{"name": "Billing"}
	first := h.do("POST", "/api/v1/groups", body, bearer(admin.Access), header("Idempotency-Key", "k-1")).expect(201)
	again := h.do("POST", "/api/v1/groups", body, bearer(admin.Access), header("Idempotency-Key", "k-1")).expect(201)
	if again.Header.Get("Idempotent-Replayed") != "true" || again.Str("id") != first.Str("id") {
		t.Fatalf("replay: %s", again.Body)
	}
	h.do("POST", "/api/v1/groups", map[string]any{"name": "Web"}, bearer(admin.Access), header("Idempotency-Key", "k-1")).expect(409, "idempotencyKeyReused")
	// A failed command releases its key for a corrected retry.
	h.do("POST", "/api/v1/groups", map[string]any{"name": ""}, bearer(admin.Access), header("Idempotency-Key", "k-2")).expect(422)
	h.do("POST", "/api/v1/groups", map[string]any{"name": ""}, bearer(admin.Access), header("Idempotency-Key", "k-2")).expect(422)
	// Keys are scoped per principal: another admin's identical key is new.
	h.do("POST", "/api/v1/users", map[string]any{"email": "a2@example.com", "givenName": "A2", "familyName": "Admin", "password": "agent password 1", "isAdmin": true}, bearer(admin.Access)).expect(201)
	a2 := h.login("a2@example.com", "agent password 1")
	h.do("POST", "/api/v1/groups", body, bearer(a2.Access), header("Idempotency-Key", "k-1")).expect(409, "groupNameTaken")
	if n := len(h.do("GET", "/api/v1/groups", nil, bearer(admin.Access)).Items()); n != 2 {
		t.Fatalf("groups: %d", n)
	}
}

func TestSettingsEndpointIsRedactedAndVersioned(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	s := h.do("GET", "/api/v1/settings", nil, bearer(admin.Access)).expect(200)
	m := s.JSON()
	if m["configVersion"].(float64) != 1 {
		t.Fatalf("settings: %s", s.Body)
	}
	smtp := m["smtp"].(map[string]any)
	smtp["password"] = "s3cret-smtp"
	delete(smtp, "passwordConfigured")
	delete(m, "configVersion")
	h.do("PUT", "/api/v1/settings", m, bearer(admin.Access), ifMatchV(1)).expect(200)
	got := h.do("GET", "/api/v1/settings", nil, bearer(admin.Access)).expect(200)
	if strings.Contains(string(got.Body), "s3cret") || got.JSON()["smtp"].(map[string]any)["passwordConfigured"] != true {
		t.Fatalf("settings not redacted: %s", got.Body)
	}
	h.do("PUT", "/api/v1/settings", m, bearer(admin.Access), ifMatchV(1)).expect(409, "versionConflict")
	h.createUser(admin, "agent@example.com", "Agent")
	agent := h.login("agent@example.com", "agent password 1")
	h.do("GET", "/api/v1/settings", nil, bearer(agent.Access)).expect(403)
}

func TestWorkflowCatalog(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.project(admin)
	st := h.do("GET", "/api/v1/statuses", nil, bearer(admin.Access)).expect(200).Items()
	if len(st) != 5 || st[0]["name"] != "Open" || st[0]["category"] != "open" || st[1]["category"] != "waitingCustomer" ||
		st[2]["category"] != "onHold" || st[1]["builtin"] != true || st[2]["builtin"] != true || st[4]["category"] != "archived" {
		t.Fatalf("statuses: %v", st)
	}
	rt := h.do("GET", "/api/v1/request-types", nil, bearer(admin.Access)).expect(200).Items()
	if len(rt) != 4 || rt[0]["name"] != "Issue" {
		t.Fatalf("request types: %v", rt)
	}
}

// Owner decision 2026-10-31: an agent transfers a ticket to another group,
// optionally naming an assignee from that group, and changes the assignee
// within the ticket's group; an assignee from another group is refused.
func TestAgentTransfersAndAssignsWithinGroup(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	triage := groupByName(h.do("GET", "/api/v1/groups", nil, bearer(admin.Access)).expect(200).Items(), "Triage")["id"].(string)
	billing := h.do("POST", "/api/v1/groups", map[string]any{"name": "Billing"}, bearer(admin.Access)).expect(201).Str("id")
	aID := h.createUser(admin, "ann@example.com", "Ann")
	bID := h.createUser(admin, "bob@example.com", "Bob")
	h.do("PUT", "/api/v1/groups/"+triage+"/members/"+aID, nil, bearer(admin.Access)).expect(200)
	h.do("PUT", "/api/v1/groups/"+billing+"/members/"+bID, nil, bearer(admin.Access)).expect(200)
	ann := h.login("ann@example.com", "agent password 1")

	tk := h.do("POST", "/api/v1/tickets", map[string]any{"subject": "Wrong queue", "body": "Please move me.",
		"requester": map[string]any{"name": "Cara", "email": "cara@example.com"}}, bearer(ann.Access)).expect(201)
	key := tk.Ref()
	if key == "" {
		key = refOf(tk.JSON()["ticket"].(map[string]any))
	}
	cur := h.do("GET", "/api/v1/tickets/"+key, nil, bearer(ann.Access)).expect(200)
	// An assignee from another group is refused.
	h.do("POST", "/api/v1/tickets/"+key+"/assignments", map[string]any{"groupId": triage, "assigneeId": bID},
		bearer(ann.Access), ifMatchV(cur.Version())).expect(422)
	// Change the assignee within the ticket's group (Ann is a member).
	res := h.do("POST", "/api/v1/tickets/"+key+"/assignments", map[string]any{"groupId": triage, "assigneeId": aID},
		bearer(ann.Access), ifMatchV(cur.Version())).expect(200)
	// Transfer to Billing with Bob as the assignee.
	out := h.do("POST", "/api/v1/tickets/"+key+"/assignments", map[string]any{"groupId": billing, "assigneeId": bID},
		bearer(ann.Access), ifMatchV(res.Version())).expect(200).JSON()
	if g, _ := out["group"].(map[string]any); g == nil || g["id"] != billing {
		t.Fatalf("group after transfer: %v", out["group"])
	}
	if a, _ := out["assignee"].(map[string]any); a == nil || a["id"] != bID {
		t.Fatalf("assignee after transfer: %v", out["assignee"])
	}
}
