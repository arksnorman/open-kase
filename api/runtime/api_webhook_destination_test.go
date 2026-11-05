package runtime

import "testing"

// TestWebhookDestinationChangeNeedsCredentials: stored write-only secrets are
// not sent to a new destination; the PATCH must enter them again.
func TestWebhookDestinationChangeNeedsCredentials(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs, other := newHookServer(t), newHookServer(t)
	path := "/api/v1/webhooks"
	created := f.do("POST", path, map[string]any{"name": "Secure", "url": hs.URL + "/hook", "eventTypes": []string{"ticket.created"},
		"auth": map[string]any{"type": "basic", "username": "u", "password": "pw"}}, bearer(f.admin.Access)).expect(201)
	id, v := created.Str("id"), created.Version()

	// A path change on the same host needs nothing.
	r := f.do("PATCH", path+"/"+id, map[string]any{"url": hs.URL + "/other"}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	v = r.Version()

	// A new host without the credentials is refused and nothing changes.
	r = f.do("PATCH", path+"/"+id, map[string]any{"url": other.URL + "/hook"}, bearer(f.admin.Access), ifMatchV(v)).expect(422)
	if !hasField(r, "auth") {
		t.Fatalf("expected an auth field error: %s", r.Body)
	}
	got := f.do("GET", path+"/"+id, nil, bearer(f.admin.Access)).expect(200)
	if got.JSON()["url"] != hs.URL+"/other" {
		t.Fatalf("url changed despite the rejection: %v", got.JSON()["url"])
	}

	// Clearing the authentication together with the move needs no re-entry.
	cleared := f.do("POST", path, map[string]any{"name": "Clear", "url": hs.URL + "/hook", "eventTypes": []string{"ticket.created"},
		"auth": map[string]any{"type": "token", "token": "t"}}, bearer(f.admin.Access)).expect(201)
	f.do("PATCH", path+"/"+cleared.Str("id"), map[string]any{"url": other.URL + "/hook", "auth": map[string]any{"type": "none"}},
		bearer(f.admin.Access), ifMatchV(cleared.Version())).expect(200)

	// With the credentials again the move succeeds.
	f.do("PATCH", path+"/"+id, map[string]any{"url": other.URL + "/hook",
		"auth": map[string]any{"type": "basic", "password": "pw2"}}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
}
