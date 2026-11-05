package runtime

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func loginBody(login, password string) map[string]string {
	return map[string]string{"login": login, "password": password}
}

func TestLoginByUsernameOrEmail(t *testing.T) {
	h := newHarness(t)

	// The bootstrap administrator's username comes from the email local part.
	r := h.do("POST", "/api/v1/auth/login", loginBody("admin", adminPassword), header("Origin", testOrigin)).expect(200)
	user := r.JSON()["user"].(map[string]any)
	if user["username"] != "admin" || user["email"] != adminEmail || r.Cookies["kase_refresh"] == nil {
		t.Fatalf("login by username: %s", r.Body)
	}
	me := h.do("GET", "/api/v1/me", nil, bearer(r.Str("accessToken"))).expect(200)
	if me.Str("username") != "admin" {
		t.Fatalf("/me without username: %s", me.Body)
	}
	// Usernames are case-insensitive; an identifier with "@" is an email.
	h.do("POST", "/api/v1/auth/login", loginBody(" ADMIN ", adminPassword)).expect(200)
	h.do("POST", "/api/v1/auth/login", loginBody("Admin@Example.com", adminPassword)).expect(200)
	// The removed "email" member is an unknown field.
	h.do("POST", "/api/v1/auth/login", map[string]string{"email": adminEmail, "password": adminPassword}).expect(422, "validationFailed")
	none := h.do("POST", "/api/v1/auth/login", map[string]string{"password": adminPassword}).expect(422, "validationFailed")
	if len(none.Problem().Errors) != 1 || none.Problem().Errors[0].Field != "login" || none.Problem().Errors[0].Code != "required" {
		t.Fatalf("no identifier: %s", none.Body)
	}
	empty := h.do("POST", "/api/v1/auth/login", map[string]string{"login": "", "password": adminPassword}).expect(422, "validationFailed")
	if empty.Problem().Errors[0].Field != "login" {
		t.Fatalf("empty login: %s", empty.Body)
	}

	// Unknown usernames, unknown emails and wrong passwords are indistinguishable.
	wrong := h.do("POST", "/api/v1/auth/login", loginBody("admin", "nope")).expect(401, "invalidCredentials")
	unknownUser := h.do("POST", "/api/v1/auth/login", loginBody("ghost", "nope")).expect(401, "invalidCredentials")
	unknownEmail := h.do("POST", "/api/v1/auth/login", loginBody("ghost@example.com", "nope")).expect(401, "invalidCredentials")
	for _, r := range []result{unknownUser, unknownEmail} {
		if r.Problem().Detail != wrong.Problem().Detail || r.Problem().Title != wrong.Problem().Title || len(r.Cookies) != 0 {
			t.Fatalf("login failures are distinguishable: %s vs %s", r.Body, wrong.Body)
		}
	}
	// A malformed username (it could never exist) fails the same way.
	h.do("POST", "/api/v1/auth/login", loginBody("1 not valid!", "nope")).expect(401, "invalidCredentials")
}

func TestLoginThrottleSharedAcrossUsernameAndEmail(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.do("POST", "/api/v1/users", map[string]any{"email": "other@example.com", "username": "otto", "givenName": "Other", "familyName": "User",
		"password": agentPassword}, bearer(admin.Access)).expect(201)

	// Alternating identifiers for one account: five failures in total
	// (fewer than five per identifier) lock the account for this client,
	// by either identifier.
	for i := 0; i < 5; i++ {
		id := "otto"
		if i%2 == 1 {
			id = "OTHER@example.com"
		}
		h.do("POST", "/api/v1/auth/login", loginBody(id, "wrong")).expect(401, "invalidCredentials")
	}
	h.do("POST", "/api/v1/auth/login", loginBody("otto", agentPassword)).expect(429, "loginThrottled")
	h.do("POST", "/api/v1/auth/login", loginBody("other@example.com", agentPassword)).expect(429, "loginThrottled")
	h.do("POST", "/api/v1/auth/login", map[string]string{"login": "other@example.com", "password": agentPassword}).expect(429, "loginThrottled")

	// Unknown usernames are throttled after the same number of failures.
	for i := 0; i < 5; i++ {
		h.do("POST", "/api/v1/auth/login", loginBody("nobody", "wrong")).expect(401, "invalidCredentials")
	}
	h.do("POST", "/api/v1/auth/login", loginBody("nobody", "wrong")).expect(429, "loginThrottled")

	// Other accounts are unaffected.
	h.do("POST", "/api/v1/auth/login", loginBody("admin", adminPassword)).expect(200)
}

func TestUsernameValidationUniquenessAndAudit(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	create := func(body map[string]any) result {
		body["givenName"], body["familyName"] = "Some", "One"
		body["password"] = agentPassword
		return h.do("POST", "/api/v1/users", body, bearer(admin.Access))
	}

	// Normalized to lower case.
	r := create(map[string]any{"email": "dana@example.com", "username": "  Dana.O  "}).expect(201)
	if r.Str("username") != "dana.o" {
		t.Fatalf("normalized username: %s", r.Body)
	}
	danaID, danaVersion := r.Str("id"), r.Version()
	h.do("POST", "/api/v1/auth/login", loginBody("DANA.O", agentPassword)).expect(200)

	for _, bad := range []string{"", "ab", "1abc", "_abc", "has space", "dana@example.com", "dañа", strings.Repeat("a", 33)} {
		p := create(map[string]any{"email": "bad@example.com", "username": bad}).expect(422, "validationFailed").Problem()
		if len(p.Errors) != 1 || p.Errors[0].Field != "username" {
			t.Fatalf("username %q: %+v", bad, p)
		}
	}
	// Reserved-looking names are allowed; 32 characters is the maximum.
	create(map[string]any{"email": "root@example.com", "username": "administrator"}).expect(201)
	create(map[string]any{"email": "long@example.com", "username": "a" + strings.Repeat("b", 31)}).expect(201)

	// Unique per workspace (case-insensitively, through normalization).
	create(map[string]any{"email": "dana2@example.com", "username": "DANA.O"}).expect(409, "usernameTaken")

	// Omitted: derived from the email local part, with a numeric suffix
	// when taken.
	if got := create(map[string]any{"email": "Dana.O@elsewhere.example"}).expect(201).Str("username"); got != "dana.o2" {
		t.Fatalf("derived username %q, want dana.o2", got)
	}
	if got := create(map[string]any{"email": "Dana.O@third.example"}).expect(201).Str("username"); got != "dana.o3" {
		t.Fatalf("derived username %q, want dana.o3", got)
	}
	if got := create(map[string]any{"email": "7+x@example.com"}).expect(201).Str("username"); got != "user-x" {
		t.Fatalf("derived username %q, want user-x", got)
	}

	// PATCH: conflict, invalid, then a change that is audited and takes
	// effect for sign-in immediately.
	h.do("PATCH", "/api/v1/users/"+danaID, map[string]any{"username": "administrator"}, bearer(admin.Access), ifMatchV(danaVersion)).expect(409, "usernameTaken")
	h.do("PATCH", "/api/v1/users/"+danaID, map[string]any{"username": "x"}, bearer(admin.Access), ifMatchV(danaVersion)).expect(422, "validationFailed")
	r = h.do("PATCH", "/api/v1/users/"+danaID, map[string]any{"username": "Dana"}, bearer(admin.Access), ifMatchV(danaVersion)).expect(200)
	if r.Str("username") != "dana" || r.Version() != danaVersion+1 {
		t.Fatalf("patched user: %s", r.Body)
	}
	h.do("POST", "/api/v1/auth/login", loginBody("dana.o", agentPassword)).expect(401, "invalidCredentials")
	h.do("POST", "/api/v1/auth/login", loginBody("dana", agentPassword)).expect(200)
	evs, err := h.c.Audit.RecentAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, e := range evs {
		if e.Action == "user.updated" && e.Target == "user/"+danaID && slices.Contains(e.Changes, "username") {
			audited = true
		}
	}
	if !audited {
		t.Fatal("username change not audited")
	}
	// The freed username can be taken by someone else.
	create(map[string]any{"email": "newcomer@example.com", "username": "dana.o"}).expect(201)

	// Listed for administrators and shown on project members.
	list := h.do("GET", "/api/v1/users?limit=100", nil, bearer(admin.Access)).expect(200)
	found := false
	for _, it := range list.Items() {
		if it["id"] == danaID && it["username"] == "dana" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user list without username: %s", list.Body)
	}
	h.project(admin)
	members := h.do("GET", "/api/v1/members", nil, bearer(admin.Access)).expect(200)
	for _, m := range members.Items() {
		if u := m["user"].(map[string]any); u["id"] == danaID && u["username"] != "dana" {
			t.Fatalf("member without username: %s", members.Body)
		}
	}
}
