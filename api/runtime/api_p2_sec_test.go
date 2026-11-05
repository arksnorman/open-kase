package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/internal/webhooks/endpoints"
	"github.com/arksnorman/open-kase/api/package/clock"
)

// Pass 2, security review. Regression tests for defects found while
// re-verifying the pass-1 fixes and the areas pass 1 did not reach.

// OC-AUTH-01 re-verification: the refresh grace window never outlives the
// session. Inside the window, the previous token yields nothing once the
// session was signed out or the account disabled, and a CSRF token that
// belongs to another session of the same user (a valid double-submit pair)
// does not unlock it.
func TestP2SecRefreshGraceEndsWithTheSession(t *testing.T) {
	h := newHarness(t)
	start := time.Now().UTC().Truncate(time.Second)
	clk := clock.NewManual(start)
	h.c.UseAuthClock(clk)
	h.h = h.router(h.c)
	rotate := func(s session) session {
		t.Helper()
		r := h.refresh(s).expect(200)
		s.Refresh, s.Access = r.Cookies["kase_refresh"].Value, r.Str("accessToken")
		return s
	}
	logout := func(s session) {
		t.Helper()
		h.do("POST", "/api/v1/auth/logout", nil, cookie("kase_refresh", s.Refresh), cookie("kase_csrf", s.CSRF),
			header("X-CSRF-Token", s.CSRF), header("Origin", testOrigin)).expect(204)
	}

	// Signed out with the successor: the previous token is dead at once.
	s0 := h.admin()
	s1 := rotate(s0)
	logout(s1)
	clk.Advance(time.Second)
	if r := h.refresh(s0).expect(401); r.JSON()["accessToken"] != nil {
		t.Fatalf("grace refresh after logout issued a token: %s", r.Body)
	}

	// Another session's CSRF pair (same user, valid cookie = header) does
	// not unlock the previous token of this session.
	a0 := h.admin()
	rotate(a0)
	other := h.admin()
	h.do("POST", "/api/v1/auth/refresh", nil, cookie("kase_refresh", a0.Refresh), cookie("kase_csrf", other.CSRF),
		header("X-CSRF-Token", other.CSRF), header("Origin", testOrigin)).expect(403, "csrfFailed")
	// Without the header, or from another origin, nothing either.
	h.do("POST", "/api/v1/auth/refresh", nil, cookie("kase_refresh", a0.Refresh), cookie("kase_csrf", a0.CSRF),
		header("Origin", testOrigin)).expect(403, "csrfFailed")
	h.do("POST", "/api/v1/auth/refresh", nil, cookie("kase_refresh", a0.Refresh), cookie("kase_csrf", a0.CSRF),
		header("X-CSRF-Token", a0.CSRF), header("Origin", "https://evil.example")).expect(403, "originRejected")
	// The rightful grace replay still works afterwards (nothing was revoked).
	h.refresh(a0).expect(200)

	// Disabled inside the window: no access token from the previous token.
	uid := h.createUser(other, "agent@example.com", "Agent")
	g0 := h.login("agent@example.com", "agent password 1")
	rotate(g0)
	v := h.do("GET", "/api/v1/users/"+uid, nil, bearer(other.Access)).expect(200).Version()
	h.do("PATCH", "/api/v1/users/"+uid, map[string]any{"status": "disabled"}, bearer(other.Access), ifMatchV(v)).expect(200)
	if r := h.refresh(g0).expect(401); r.JSON()["accessToken"] != nil {
		t.Fatalf("grace refresh of a disabled user issued a token: %s", r.Body)
	}
	if clk.Now().Sub(start) >= auth.RefreshGrace {
		t.Fatal("test left the grace window; the checks above did not exercise it")
	}
}

// redactingTickets wraps the webhook worker's ticket reads: right after the
// worker has read a message for a payload, the hook runs once (the moment
// an administrator's redaction commits), before the payload is stored.
type redactingTickets struct {
	notifications.Tickets
	hook func()
}

func (r *redactingTickets) GetMessage(ctx context.Context, scope *access.ProjectScope, ticketRef, messageID string) (tickets.Ticket, tickets.Message, error) {
	t, m, err := r.Tickets.GetMessage(ctx, scope, ticketRef, messageID)
	if h := r.hook; h != nil {
		r.hook = nil
		h()
	}
	return t, m, err
}

// OC-ASYNC-07: a redaction that commits while the webhook worker builds a
// payload (after it read the message, before it stored the bytes) dropped
// nothing, because nothing was stored yet; the worker then stored the old
// text, and every retry (automatic or manual) re-sent it after the
// redaction. The worker now stores and sends a payload only when no
// redaction (or grant withdrawal) happened since it started building it.
func TestP2SecRedactionDuringPayloadBuildIsNotStoredOrResent(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	down := newHookServer(t)
	down.respond = func(int, []byte) int { return 410 } // dead-letters at once
	path := "/api/v1/webhooks"
	ep := f.do("POST", path, map[string]any{"name": "Test hook", "url": down.URL, "eventTypes": []string{"ticket.message.public"}},
		bearer(f.admin.Access)).expect(201).Str("id")
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.drain()
	before := len(down.all())

	wrapped := &redactingTickets{Tickets: f.c.Tickets}
	f.c.Deliverer = endpoints.NewDeliverer(endpoints.DelivererDeps{Jobs: f.c.Jobs, Deliveries: f.c.NotifyRepo, Events: f.c.NotifyRepo,
		Endpoints: f.c.WebhooksRepo, Access: f.c.Access, Tickets: wrapped, Keys: f.c.Keys,
		Policy: f.c.webhookPolicy, Now: f.c.Clock.Now, Jitter: func(time.Duration) time.Duration { return 0 }})
	mid := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "my password is pineapple42"},
		bearer(f.integration)).expect(201).Str("id")
	wrapped.hook = func() {
		f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "Customer posted a password"}, bearer(f.admin.Access)).expect(200)
	}
	f.drain()
	if wrapped.hook != nil {
		t.Fatal("the worker never read the message")
	}

	// No delivery record keeps the redacted text ...
	cur, err := f.coll("notificationDeliveries").Find(context.Background(), bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Status string `bson:"status"`
		Body   []byte `bson:"body"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(string(row.Body), "pineapple42") {
			t.Errorf("a %s delivery stores the text redacted while its payload was built", row.Status)
		}
	}
	// ... and once the destination is back, a retry of the failed delivery
	// sends the redacted message, not the old text.
	down.respond = func(int, []byte) int { return 204 }
	for _, d := range f.do("GET", path+"/"+ep+"/deliveries?status=failed", nil, bearer(f.admin.Access)).expect(200).Items() {
		f.do("POST", path+"/"+ep+"/deliveries/"+d["id"].(string)+"/retry", nil, bearer(f.admin.Access)).expect(202)
	}
	f.drain()
	reqs := down.all()[before:]
	if len(reqs) == 0 {
		t.Fatal("no delivery reached the destination")
	}
	last := string(reqs[len(reqs)-1].body)
	if strings.Contains(last, "pineapple42") || !strings.Contains(last, "[redacted]") {
		t.Fatalf("delivery after the redaction sent: %s", last)
	}
}
