package runtime

import (
	"testing"
)

// TestLastAdminGuardHoldsUnderConcurrentDemotions: two administrators remove
// each other's administrator role at the same moment. Each transaction
// counted the remaining administrators on its own snapshot, so both could
// pass and leave the installation without any administrator (OC-AUTH-03).
// The guard must let at most one of the two changes through, in every round.
func TestLastAdminGuardHoldsUnderConcurrentDemotions(t *testing.T) {
	h := newHarness(t)
	a := h.admin()
	h.do("POST", "/api/v1/users", map[string]any{"email": "second@example.com", "givenName": "Second", "familyName": "Admin", "password": adminPassword,
		"isAdmin": true}, bearer(a.Access)).expect(201)
	b := h.login("second@example.com", adminPassword)
	user := func(by session, id string) result {
		t.Helper()
		return h.do("GET", "/api/v1/users/"+id, nil, bearer(by.Access)).expect(200)
	}
	setAdmin := func(by session, id string, v int64, admin bool) result {
		return h.do("PATCH", "/api/v1/users/"+id, map[string]any{"isAdmin": admin}, bearer(by.Access), ifMatchV(v))
	}
	admins := func(by session) int {
		t.Helper()
		n := 0
		for _, u := range h.do("GET", "/api/v1/users?limit=100", nil, bearer(by.Access)).expect(200).Items() {
			if u["isAdmin"] == true && u["status"] == "active" {
				n++
			}
		}
		return n
	}
	for round := 0; round < 25; round++ {
		va, vb := user(a, a.UserId).Version(), user(b, b.UserId).Version()
		res := make(chan result, 2)
		go func() { res <- setAdmin(a, b.UserId, vb, false) }()
		go func() { res <- setAdmin(b, a.UserId, va, false) }()
		ok := 0
		for _, r := range []result{<-res, <-res} {
			switch r.Code {
			case 200:
				ok++
			case 409:
			default:
				t.Fatalf("round %d: unexpected %d %s", round, r.Code, r.Body)
			}
		}
		if ok == 2 {
			t.Fatalf("round %d: both administrators demoted each other; the installation has no administrator", round)
		}
		// Whoever is still an administrator restores the other one.
		keeper, other := a, b
		if h.do("GET", "/api/v1/users", nil, bearer(a.Access)).Code == 403 {
			keeper, other = b, a
		}
		if n := admins(keeper); n != 2-ok {
			t.Fatalf("round %d: %d administrators after %d successful demotion(s)", round, n, ok)
		}
		if r := user(keeper, other.UserId); r.JSON()["isAdmin"] == false {
			setAdmin(keeper, other.UserId, r.Version(), true).expect(200)
		}
		if n := admins(a); n != 2 {
			t.Fatalf("round %d: %d administrators after restore", round, n)
		}
	}
}
