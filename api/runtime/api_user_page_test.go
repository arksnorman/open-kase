package runtime

import (
	"testing"
)

// User names are given, middle (optional) and family name; the read-only
// name is derived from them and appears wherever a user is shown.
func TestUserNameParts(t *testing.T) {
	f := newTicketFixture(t)
	r := f.do("POST", "/api/v1/users", map[string]any{"email": "ada@example.com", "givenName": " Ada ", "middleName": "B.",
		"familyName": "Lovelace", "password": agentPassword}, bearer(f.admin.Access)).expect(201)
	if r.Str("name") != "Ada B. Lovelace" || r.Str("givenName") != "Ada" || r.Str("middleName") != "B." || r.Str("familyName") != "Lovelace" {
		t.Fatalf("create: %s", r.Body)
	}
	ada := r.Str("id")
	r = f.do("POST", "/api/v1/users", map[string]any{"email": "alan@example.com", "givenName": "Alan", "familyName": "Turing"},
		bearer(f.admin.Access)).expect(201)
	if r.Str("name") != "Alan Turing" || r.Str("middleName") != "" {
		t.Fatalf("create without middle name: %s", r.Body)
	}
	// Given and family names are required; the old single name field is unknown.
	f.do("POST", "/api/v1/users", map[string]any{"email": "x1@example.com", "familyName": "X"}, bearer(f.admin.Access)).expect(422, "validationFailed")
	f.do("POST", "/api/v1/users", map[string]any{"email": "x2@example.com", "givenName": "X"}, bearer(f.admin.Access)).expect(422, "validationFailed")
	f.do("POST", "/api/v1/users", map[string]any{"email": "x3@example.com", "givenName": "X", "familyName": "  "}, bearer(f.admin.Access)).expect(422, "validationFailed")
	f.do("POST", "/api/v1/users", map[string]any{"email": "x4@example.com", "givenName": "X\tY", "familyName": "Z"}, bearer(f.admin.Access)).expect(422, "validationFailed")
	f.do("POST", "/api/v1/users", map[string]any{"email": "x5@example.com", "name": "X Y"}, bearer(f.admin.Access)).expect(422, "validationFailed")

	// Admin edit: change one part, clear the middle name; family name cannot be emptied.
	v := f.do("GET", "/api/v1/users/"+ada, nil, bearer(f.admin.Access)).expect(200).Version()
	r = f.do("PATCH", "/api/v1/users/"+ada, map[string]any{"givenName": "Augusta", "middleName": ""}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	if r.Str("name") != "Augusta Lovelace" || r.Str("middleName") != "" {
		t.Fatalf("patch: %s", r.Body)
	}
	f.do("PATCH", "/api/v1/users/"+ada, map[string]any{"familyName": ""}, bearer(f.admin.Access), ifMatchV(r.Version())).expect(422, "validationFailed")
	f.do("PATCH", "/api/v1/users/"+ada, map[string]any{"name": "New Name"}, bearer(f.admin.Access), ifMatchV(r.Version())).expect(422, "validationFailed")

	// The derived name appears as the ticket assignee.
	ines := f.agent(f.admin, "ines@example.com", "Ines Maria Duarte", "agent", f.triage)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(ines.Access), ifMatchV(f.version(key))).expect(200)
	if a := obj(f.get(f.admin.Access, key).expect(200).JSON(), "assignee"); a["name"] != "Ines Maria Duarte" {
		t.Fatalf("assignee: %v", a)
	}
}

// An administrator reads and sets a user's groups in one call; removals
// release ineligible assignments like the group member routes; agents get 403.
func TestUserGroupsEndpoint(t *testing.T) {
	f := newTicketFixture(t)
	path := "/api/v1/users/" + f.marco.UserId + "/groups"
	names := func(r result) []string {
		var out []string
		for _, g := range r.JSON()["groups"].([]any) {
			out = append(out, g.(map[string]any)["name"].(string))
		}
		return out
	}
	if got := names(f.do("GET", path, nil, bearer(f.admin.Access)).expect(200)); len(got) != 2 || got[0] != "Delivery" || got[1] != "Triage" {
		t.Fatalf("marco's groups: %v", got)
	}
	f.do("GET", path, nil, bearer(f.dana.Access)).expect(403)
	f.do("PUT", path, map[string]any{"groupIds": []string{f.triage}}, bearer(f.dana.Access)).expect(403)
	f.do("GET", "/api/v1/users/0123456789abcdef01234567/groups", nil, bearer(f.admin.Access)).expect(404, "userNotFound")
	f.do("PUT", "/api/v1/users/0123456789abcdef01234567/groups", map[string]any{"groupIds": []string{}}, bearer(f.admin.Access)).expect(404, "userNotFound")
	f.do("PUT", path, map[string]any{}, bearer(f.admin.Access)).expect(422, "validationFailed")

	// Invalid input changes nothing (an unknown group, an inactive group).
	f.do("PUT", path, map[string]any{"groupIds": []string{"0123456789abcdef01234567"}}, bearer(f.admin.Access)).expect(404, "groupNotFound")
	old := f.do("POST", "/api/v1/groups", map[string]any{"name": "Old"}, bearer(f.admin.Access)).expect(201).Str("id")
	f.do("PATCH", "/api/v1/groups/"+old, map[string]any{"active": false}, bearer(f.admin.Access), ifMatchV(1)).expect(200)
	f.do("PUT", path, map[string]any{"groupIds": []string{old}}, bearer(f.admin.Access)).expect(409, "groupInactive")
	if got := names(f.do("GET", path, nil, bearer(f.admin.Access)).expect(200)); len(got) != 2 {
		t.Fatalf("groups changed by invalid input: %v", got)
	}

	// Marco holds a Delivery ticket; leaving Delivery releases it.
	key := f.createTicket(f.integration, map[string]any{"groupId": f.delivery}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.marco.Access), ifMatchV(f.version(key))).expect(200)
	if got := names(f.do("PUT", path, map[string]any{"groupIds": []string{f.triage, f.triage}}, bearer(f.admin.Access)).expect(200)); len(got) != 1 || got[0] != "Triage" {
		t.Fatalf("after removal: %v", got)
	}
	f.drain()
	if tk := f.get(f.admin.Access, key).expect(200).JSON(); tk["assignee"] != nil {
		t.Fatalf("ineligible assignee kept: %v", tk["assignee"])
	}
	// Marco no longer sees Delivery tickets at all.
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.marco.Access), ifMatchV(f.version(key))).expect(404, "ticketNotFound")

	// Adding: Dana joins Delivery as well; the directory shows it.
	if got := names(f.do("PUT", "/api/v1/users/"+f.dana.UserId+"/groups", map[string]any{"groupIds": []string{f.delivery, f.triage}},
		bearer(f.admin.Access)).expect(200)); len(got) != 2 {
		t.Fatalf("dana's groups: %v", got)
	}
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
}
