package runtime

import (
	"strings"
	"testing"
)

// Update endpoints used by the UI's inline edit fields (2026-10-02):
// group name/description, own display name and integration name. The
// project has no name, description or key to edit any more (owner decision
// 2026-10-31): the product name and description are settings.

func TestPatchProjectOnlyRequesterAcknowledgement(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.project(admin)
	h.createUser(admin, "agent2@example.com", "Agent Two")
	agent := h.login("agent2@example.com", "agent password 1")

	for _, field := range []string{"name", "description", "key"} {
		h.do("PATCH", "/api/v1/project", map[string]any{field: "X"}, bearer(admin.Access), ifMatchV(1)).expect(422, "validationFailed")
	}
	h.do("PATCH", "/api/v1/project", map[string]any{"requesterAcknowledgement": false}, bearer(agent.Access), ifMatchV(1)).expect(403, "forbidden")
	r := h.do("PATCH", "/api/v1/project", map[string]any{"requesterAcknowledgement": false}, bearer(admin.Access), ifMatchV(1)).expect(200)
	if r.JSON()["requesterAcknowledgement"] != false || r.Version() != 2 {
		t.Fatalf("patch: %s", r.Body)
	}
	if _, ok := r.JSON()["key"]; ok {
		t.Fatalf("project still exposes key: %s", r.Body)
	}

	// The product description is a setting; GET /project names the product.
	settings := h.do("GET", "/api/v1/settings", nil, bearer(admin.Access)).expect(200)
	doc := settings.JSON()
	if doc["updatedAt"] == nil {
		t.Fatalf("settings without updatedAt: %s", settings.Body)
	}
	strip := func(doc map[string]any) {
		delete(doc, "configVersion")
		delete(doc["smtp"].(map[string]any), "passwordConfigured")
	}
	strip(doc)
	doc["product"].(map[string]any)["name"] = "Acme Help"
	doc["product"].(map[string]any)["description"] = "  Mailbox support  "
	put := h.do("PUT", "/api/v1/settings", doc, bearer(admin.Access), header("If-Match", jsonString(settings.JSON()["configVersion"]))).expect(200)
	if p := obj(put.JSON(), "product"); p["description"] != "Mailbox support" || p["name"] != "Acme Help" {
		t.Fatalf("settings: %s", put.Body)
	}
	if _, err := h.c.Settings.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.do("GET", "/api/v1/project", nil, bearer(agent.Access)).expect(200); got.Str("name") != "Acme Help" {
		t.Fatalf("project name: %s", got.Body)
	}
	// Omitting the description keeps it.
	doc = put.JSON()
	strip(doc)
	delete(doc["product"].(map[string]any), "description")
	put = h.do("PUT", "/api/v1/settings", doc, bearer(admin.Access), header("If-Match", jsonString(put.JSON()["configVersion"]))).expect(200)
	if obj(put.JSON(), "product")["description"] != "Mailbox support" {
		t.Fatalf("description dropped: %s", put.Body)
	}
	doc = put.JSON()
	strip(doc)
	doc["product"].(map[string]any)["description"] = strings.Repeat("x", 1001)
	h.do("PUT", "/api/v1/settings", doc, bearer(admin.Access), header("If-Match", jsonString(put.JSON()["configVersion"]))).expect(422, "validationFailed")
}

func TestPatchGroupNameUniquenessAndTriage(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.project(admin)
	h.createUser(admin, "agent@example.com", "Agent")
	agent := h.login("agent@example.com", "agent password 1")

	triage := groupByName(h.do("GET", "/api/v1/groups", nil, bearer(admin.Access)).expect(200).Items(), "Triage")
	tid, tv := triage["id"].(string), int64(triage["version"].(float64))
	billing := h.do("POST", "/api/v1/groups", map[string]any{"name": "Billing"}, bearer(admin.Access)).expect(201)
	bid := billing.Str("id")

	// Rename + describe.
	r := h.do("PATCH", "/api/v1/groups/"+bid, map[string]any{"name": "Invoices", "description": "Billing questions"}, bearer(admin.Access), ifMatchV(1)).expect(200)
	if r.Str("name") != "Invoices" || r.Str("description") != "Billing questions" || r.Version() != 2 {
		t.Fatalf("rename: %s", r.Body)
	}
	// Unique (case/space-insensitive) → 409 groupNameTaken.
	h.do("PATCH", "/api/v1/groups/"+bid, map[string]any{"name": " triage"}, bearer(admin.Access), ifMatchV(2)).expect(409, "groupNameTaken")
	// Stale version, validation, permission.
	h.do("PATCH", "/api/v1/groups/"+bid, map[string]any{"name": "Again"}, bearer(admin.Access), ifMatchV(1)).expect(409, "versionConflict")
	h.do("PATCH", "/api/v1/groups/"+bid, map[string]any{"name": "   "}, bearer(admin.Access), ifMatchV(2)).expect(422, "validationFailed")
	h.do("PATCH", "/api/v1/groups/"+bid, map[string]any{"name": "Agent pick"}, bearer(agent.Access), ifMatchV(2)).expect(403, "forbidden")

	// Triage: the label may be renamed; it stays protected and the default group.
	tr := h.do("PATCH", "/api/v1/groups/"+tid, map[string]any{"name": "Inbox"}, bearer(admin.Access), ifMatchV(tv)).expect(200)
	if tr.Str("name") != "Inbox" || tr.JSON()["protected"] != true {
		t.Fatalf("triage rename: %s", tr.Body)
	}
	h.do("PATCH", "/api/v1/groups/"+tid, map[string]any{"active": false}, bearer(admin.Access), ifMatchV(tr.Version())).expect(409, "groupProtected")
	h.do("DELETE", "/api/v1/groups/"+tid, nil, bearer(admin.Access)).expect(409, "groupProtected")
	if p := h.do("GET", "/api/v1/project", nil, bearer(admin.Access)).expect(200); p.Str("defaultGroupId") != tid {
		t.Fatalf("default group changed: %s", p.Body)
	}
	// The old name is free again.
	h.do("PATCH", "/api/v1/groups/"+bid, map[string]any{"name": "Triage"}, bearer(admin.Access), ifMatchV(2)).expect(200)
	if !contains(h.auditActions(), "group.updated:success") {
		t.Fatal("missing group.updated audit")
	}
}

func TestPatchMeDisplayName(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.createUser(admin, "dana@example.com", "Dana")
	dana := h.login("dana@example.com", "agent password 1")

	me := h.do("GET", "/api/v1/me", nil, bearer(dana.Access)).expect(200)
	v := me.Version()
	if v < 1 {
		t.Fatalf("me has no version: %s", me.Body)
	}
	h.do("PATCH", "/api/v1/me", map[string]any{"familyName": "O."}, bearer(dana.Access)).expect(428)
	r := h.do("PATCH", "/api/v1/me", map[string]any{"familyName": " O. "}, bearer(dana.Access), ifMatchV(v)).expect(200)
	if r.Str("name") != "Dana O." || r.Str("givenName") != "Dana" || r.Str("familyName") != "O." || r.Version() != v+1 || r.Str("email") != "dana@example.com" {
		t.Fatalf("patch me: %s", r.Body)
	}
	h.do("PATCH", "/api/v1/me", map[string]any{"givenName": "Stale"}, bearer(dana.Access), ifMatchV(v)).expect(409, "versionConflict")
	h.do("PATCH", "/api/v1/me", map[string]any{"givenName": ""}, bearer(dana.Access), ifMatchV(v+1)).expect(422, "validationFailed")
	h.do("PATCH", "/api/v1/me", map[string]any{"familyName": "   "}, bearer(dana.Access), ifMatchV(v+1)).expect(422, "validationFailed")
	// The old single name field is no longer accepted.
	h.do("PATCH", "/api/v1/me", map[string]any{"name": "Dana O."}, bearer(dana.Access), ifMatchV(v+1)).expect(422, "validationFailed")
	// Only the name is self-service: roles, status and username are unknown fields here.
	h.do("PATCH", "/api/v1/me", map[string]any{"isAdmin": true}, bearer(dana.Access), ifMatchV(v+1)).expect(422, "validationFailed")
	h.do("PATCH", "/api/v1/me", map[string]any{"username": "boss"}, bearer(dana.Access), ifMatchV(v+1)).expect(422, "validationFailed")
	h.do("PATCH", "/api/v1/me", map[string]any{"givenName": "Dana"}, header("Origin", testOrigin)).expect(401)

	// The administrator view sees the change; the session stays valid.
	users := h.do("GET", "/api/v1/users", nil, bearer(admin.Access)).expect(200).Items()
	found := false
	for _, u := range users {
		if u["email"] == "dana@example.com" {
			found = u["name"] == "Dana O."
		}
	}
	if !found {
		t.Fatalf("admin view: %v", users)
	}
	h.do("GET", "/api/v1/me", nil, bearer(dana.Access)).expect(200)
	if !contains(h.auditActions(), "user.profileUpdated:success") {
		t.Fatal("missing user.profileUpdated audit")
	}
}

func TestPatchIntegrationName(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.project(admin)
	h.createUser(admin, "agent@example.com", "Agent")
	agent := h.login("agent@example.com", "agent password 1")

	created := h.do("POST", "/api/v1/integrations", map[string]any{"name": "Gateway", "scopes": []string{"tickets:read"}}, bearer(admin.Access)).expect(201)
	id, token := created.Str("id"), created.Str("token")
	if created.Version() != 1 {
		t.Fatalf("version missing: %s", created.Body)
	}
	path := "/api/v1/integrations/" + id

	r := h.do("PATCH", path, map[string]any{"name": "Billing portal"}, bearer(admin.Access), ifMatchV(1)).expect(200)
	if r.Str("name") != "Billing portal" || r.Version() != 2 || len(r.JSON()["scopes"].([]any)) != 1 {
		t.Fatalf("rename: %s", r.Body)
	}
	// Scopes are not editable; validation, stale version and permission.
	h.do("PATCH", path, map[string]any{"scopes": []string{"tickets:create"}}, bearer(admin.Access), ifMatchV(2)).expect(422, "validationFailed")
	h.do("PATCH", path, map[string]any{}, bearer(admin.Access), ifMatchV(2)).expect(422, "validationFailed")
	h.do("PATCH", path, map[string]any{"name": ""}, bearer(admin.Access), ifMatchV(2)).expect(422, "validationFailed")
	h.do("PATCH", path, map[string]any{"name": "Old"}, bearer(admin.Access), ifMatchV(1)).expect(409, "versionConflict")
	h.do("PATCH", path, map[string]any{"name": "Agent"}, bearer(agent.Access), ifMatchV(2)).expect(403, "forbidden")
	h.do("PATCH", path, map[string]any{"name": "Self"}, bearer(token), ifMatchV(2)).expect(403)
	h.do("PATCH", "/api/v1/integrations/0123456789abcdef01234567", map[string]any{"name": "X"}, bearer(admin.Access), ifMatchV(1)).expect(404, "integrationNotFound")

	// The token keeps working under the new name.
	h.do("GET", "/api/v1/project", nil, bearer(token)).expect(200)

	// Revoked integrations cannot be renamed.
	h.do("DELETE", path, nil, bearer(admin.Access)).expect(204)
	h.do("PATCH", path, map[string]any{"name": "Gone"}, bearer(admin.Access), ifMatchV(3)).expect(409, "integrationRevoked")
	if !contains(h.auditActions(), "integration.updated:success") {
		t.Fatal("missing integration.updated audit")
	}
}
