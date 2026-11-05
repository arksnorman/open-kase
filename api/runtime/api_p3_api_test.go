package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/internal/webhooks/endpoints"
)

// disconnectingWriter cancels the request's context as soon as the handler
// starts writing its response: the command has committed by then, and the
// client is gone (timeout, closed tab, proxy dropping the connection)
// before the server has recorded the outcome.
type disconnectingWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *disconnectingWriter) WriteHeader(code int) {
	w.cancel()
	w.ResponseRecorder.WriteHeader(code)
}

func (w *disconnectingWriter) Write(b []byte) (int, error) {
	w.cancel()
	return w.ResponseRecorder.Write(b)
}

// doDisconnecting sends one request whose client disconnects right after
// the handler committed (see disconnectingWriter).
func (h *harness) doDisconnecting(method, path string, body any, opts ...reqOpt) result {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for _, o := range opts {
		o(req)
	}
	w := &disconnectingWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	h.h.ServeHTTP(w, req)
	return result{t: h.t, Code: w.Code, Body: w.Body.Bytes(), Header: w.Header()}
}

// Pass-3 review (P2-REL unverified concern, confirmed): the idempotency
// middleware recorded the outcome with the request's context. A client that
// disconnected right after the command committed left the reservation
// pending: its retry with the same key answered 409 idempotencyInProgress
// for up to a minute and then ran the command again (a second macro here;
// a uniqueness conflict for keyed resources). A failed command's
// reservation was likewise not released, so an immediate retry got 409.
func TestP3IdempotencyOutcomeRecordedAfterClientDisconnect(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	h.project(admin)
	body := map[string]any{"name": "Greeting", "body": "Hello, thanks for writing in."}
	key := header("Idempotency-Key", "macro-greeting-1")

	first := h.doDisconnecting("POST", "/api/v1/macros", body, bearer(admin.Access), key).expect(201)
	id := first.Str("id")

	// The retry replays the committed result at once...
	again := h.do("POST", "/api/v1/macros", body, bearer(admin.Access), key)
	if again.Code != 201 || again.Header.Get("Idempotent-Replayed") != "true" || again.Str("id") != id {
		t.Errorf("immediate retry: status %d replayed=%q id=%q, want 201 replay of %s: %s",
			again.Code, again.Header.Get("Idempotent-Replayed"), again.Str("id"), id, again.Body)
	}
	// ...and after the reservation lease (1 minute) as well, without running
	// the command a second time.
	db := h.rawDB()
	ctx := context.Background()
	if _, err := db.Collection("idempotencyRecords").UpdateMany(ctx, bson.D{{Key: "key", Value: "macro-greeting-1"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "leaseUntil", Value: time.Now().Add(-time.Hour)}}}}); err != nil {
		t.Fatal(err)
	}
	later := h.do("POST", "/api/v1/macros", body, bearer(admin.Access), key)
	if later.Code != 201 || later.Header.Get("Idempotent-Replayed") != "true" || later.Str("id") != id {
		t.Errorf("retry after the lease: status %d replayed=%q id=%q, want 201 replay of %s: %s",
			later.Code, later.Header.Get("Idempotent-Replayed"), later.Str("id"), id, later.Body)
	}
	if n, err := db.Collection("macros").CountDocuments(ctx, bson.D{{Key: "name", Value: "Greeting"}}); err != nil || n != 1 {
		t.Errorf("macros named Greeting: %d (err %v), want 1", n, err)
	}

	// A command that failed releases its reservation even when the client
	// is gone: the retry runs again (and fails the same way) instead of
	// answering idempotencyInProgress for a minute.
	bad := map[string]any{"name": "No content"}
	badKey := header("Idempotency-Key", "macro-bad-1")
	h.doDisconnecting("POST", "/api/v1/macros", bad, bearer(admin.Access), badKey).expect(422)
	if r := h.do("POST", "/api/v1/macros", bad, bearer(admin.Access), badKey); r.Code != http.StatusUnprocessableEntity {
		t.Errorf("retry of a failed command: status %d, want 422: %s", r.Code, r.Body)
	}
}

// OC-OPS-10: the ticket-scoped writes on notification deliveries and outbox
// events (the redaction transaction) and the
// grant-withdrawal update read the whole collection. The ticket index of
// both collections (migration 10) is partial on {ticketId: {$type:
// "objectId"}}, which a plain ID equality cannot use, and the endpoint
// filter lacked the project prefix of its index. Every such write must now
// be served by an index (checked with this database's profiler).
func TestP3TicketScopedDeliveryAndOutboxWritesUseIndexes(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hook := newHookServer(t)
	path := "/api/v1/webhooks"
	created := f.do("POST", path, map[string]any{"name": "Test hook", "url": hook.URL, "includeAgentNotes": true,
		"eventTypes": []string{"ticket.message.public"}}, bearer(f.admin.Access)).expect(201).JSON()
	id, v := created["id"].(string), int64(created["version"].(float64))
	tk := f.createTicket(f.integration, map[string]any{}).JSON()
	key := refOf(tk)
	mid := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "my password is pineapple42"},
		bearer(f.integration)).expect(201).Str("id")
	f.drain()

	// Other tickets' deliveries: a scan (or the channel index) reads them all.
	ctx := context.Background()
	db := f.rawDB()
	now := time.Now()
	var others []any
	for i := 0; i < 300; i++ {
		others = append(others, bson.D{{Key: "workspaceId", Value: bson.NewObjectID()}, {Key: "projectId", Value: bson.NewObjectID()},
			{Key: "ticketId", Value: bson.NewObjectID()}, {Key: "channel", Value: "webhook"}, {Key: "status", Value: "retrying"},
			{Key: "attempts", Value: int32(1)}, {Key: "createdAt", Value: now}, {Key: "updatedAt", Value: now}})
	}
	if _, err := db.Collection("notificationDeliveries").InsertMany(ctx, others); err != nil {
		t.Fatal(err)
	}

	// Profile this throwaway database only.
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 2}}).Err(); err != nil {
		t.Fatal(err)
	}
	f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "Customer posted a password"}, bearer(f.admin.Access)).expect(200)
	f.do("PATCH", path+"/"+id, map[string]any{"includeAgentNotes": false}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 0}}).Err(); err != nil {
		t.Fatal(err)
	}

	cur, err := db.Collection("system.profile").Find(ctx, bson.D{
		{Key: "ns", Value: bson.D{{Key: "$in", Value: bson.A{db.Name() + ".notificationDeliveries", db.Name() + ".outbox"}}}},
		{Key: "op", Value: bson.D{{Key: "$in", Value: bson.A{"update", "remove"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		NS       string   `bson:"ns"`
		Op       string   `bson:"op"`
		Plan     string   `bson:"planSummary"`
		Examined int64    `bson:"docsExamined"`
		Command  bson.Raw `bson:"command"`
	}
	if err := cur.All(ctx, &entries); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range entries {
		seen[strings.TrimPrefix(e.NS, db.Name()+".")+" "+e.Op]++
		if strings.HasPrefix(e.Plan, "COLLSCAN") || e.Examined > 20 {
			t.Errorf("%s %s read %d documents (%s): %s", e.NS, e.Op, e.Examined, e.Plan, e.Command.Lookup("q"))
		}
	}
	// Redaction (stored bodies, body generation) and grant withdrawal update
	// deliveries.
	if seen["notificationDeliveries update"] < 3 {
		t.Fatalf("profiled writes: %v", seen)
	}
}

// grantWithdrawingTickets runs hook once when the webhook worker reads the
// raw ticket for the assignee's name (a ticket.assigned payload built with
// the agent-note grant): the moment an administrator withdraws the grant.
type grantWithdrawingTickets struct {
	notifications.Tickets
	hook func()
}

func (g *grantWithdrawingTickets) SystemTicket(ctx context.Context, projectID, ticketID string) (tickets.Ticket, error) {
	t, err := g.Tickets.SystemTicket(ctx, projectID, ticketID)
	if h := g.hook; h != nil {
		g.hook = nil
		h()
	}
	return t, err
}

// OC-ASYNC-07, grant-withdrawal path (pass-3 re-verification, also covering
// the OC-OPS-10 filter change): the agent-note grant is withdrawn while the
// worker builds a ticket.assigned payload that names the assignee. That
// payload is neither stored nor sent; the rebuilt one names no staff.
func TestP3GrantWithdrawnDuringPayloadBuildIsNotStoredOrSent(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hook := newHookServer(t)
	path := "/api/v1/webhooks"
	created := f.do("POST", path, map[string]any{"name": "Test hook", "url": hook.URL, "includeAgentNotes": true,
		"eventTypes": []string{"ticket.assigned"}}, bearer(f.admin.Access)).expect(201).JSON()
	id, v := created["id"].(string), int64(created["version"].(float64))
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.drain()

	wrapped := &grantWithdrawingTickets{Tickets: f.c.Tickets}
	f.c.Deliverer = endpoints.NewDeliverer(endpoints.DelivererDeps{Jobs: f.c.Jobs, Deliveries: f.c.NotifyRepo, Events: f.c.NotifyRepo,
		Endpoints: f.c.WebhooksRepo, Access: f.c.Access, Tickets: wrapped, Keys: f.c.Keys,
		Policy: f.c.webhookPolicy, Now: f.c.Clock.Now, Jitter: func(time.Duration) time.Duration { return 0 }})
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.marco.Access), ifMatchV(f.version(key))).expect(200)
	wrapped.hook = func() {
		f.do("PATCH", path+"/"+id, map[string]any{"includeAgentNotes": false}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	}
	f.drain()
	if wrapped.hook != nil {
		t.Fatal("the worker never built a payload naming the assignee")
	}
	reqs := hook.all()
	if len(reqs) == 0 {
		t.Fatal("the rebuilt payload was not delivered")
	}
	for _, r := range reqs {
		if strings.Contains(string(r.body), f.marco.UserId) || strings.Contains(string(r.body), "Marco") {
			t.Fatalf("staff identity sent after the grant was withdrawn: %s", r.body)
		}
	}
	cur, err := f.coll("notificationDeliveries").Find(context.Background(), bson.D{{Key: "channel", Value: "webhook"}})
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Body []byte `bson:"body"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(string(row.Body), f.marco.UserId) {
			t.Fatalf("a stored payload names the assignee after the grant was withdrawn: %s", row.Body)
		}
	}
}

// OC-OPS-08 re-verification (pass 3): every list view pages exactly like a
// single page (same tickets, same order, none skipped or repeated) with
// only three distinct update times, deleted tickets interleaved, mixed
// statuses and leftover assignments, for an administrator, an agent in both
// groups, an agent in one small group who also holds leftover assignments,
// and an integration (public order). The All view of the administrator and
// the integration reads in index order (migration 13); the restricted
// agent's cost per page is logged.
func TestP3ListViewsPageLikeOnePage(t *testing.T) {
	f := newTicketFixture(t)
	ctx := context.Background()
	p, err := f.c.Integrations.Authenticate(ctx, f.integration)
	if err != nil {
		t.Fatal(err)
	}
	intg, err := f.c.Access.Scope(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	const n = 140
	name, email := "Customer", "customer@example.com"
	ids := make([]bson.ObjectID, n)
	for i := 0; i < n; i++ {
		group := f.delivery // Dana's group (Triage) holds one ticket in ten
		if i%10 == 0 {
			group = f.triage
		}
		tk, err := f.c.Tickets.Create(ctx, intg, tickets.CreateInput{Subject: fmt.Sprintf("Mailbox %d bounces", i), Body: "Bounced.",
			Requester: tickets.RequesterInput{Name: &name, Email: &email}, GroupId: &group})
		if err != nil {
			t.Fatal(err)
		}
		ids[i], _ = bson.ObjectIDFromHex(tk.Id)
		if i%9 == 0 {
			f.do("PUT", tpath(tnum(tk), "/watchers/me"), nil, bearer(f.marco.Access)).expect(200)
		}
	}
	dana, _ := bson.ObjectIDFromHex(f.dana.UserId)
	marco, _ := bson.ObjectIDFromHex(f.marco.UserId)
	base := time.Now().UTC().Truncate(time.Millisecond)
	col := f.coll("tickets")
	for i, id := range ids {
		set := bson.D{{Key: "updatedAt", Value: base.Add(-time.Duration(i%3) * time.Millisecond)},
			{Key: "publicUpdatedAt", Value: base.Add(-time.Duration((i+1)%3) * time.Millisecond)}}
		switch i % 7 {
		case 1:
			set = append(set, bson.E{Key: "statusCategory", Value: "waitingCustomer"})
		case 2:
			set = append(set, bson.E{Key: "statusCategory", Value: "resolved"})
		case 3:
			set = append(set, bson.E{Key: "statusCategory", Value: "archived"})
		}
		switch {
		case i%5 == 2 && i%10 != 0: // left over from Delivery, which Dana is not in
			set = append(set, bson.E{Key: "assigneeId", Value: dana})
		case i%5 == 3:
			set = append(set, bson.E{Key: "assigneeId", Value: marco})
		}
		if _, err := col.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: set}}); err != nil {
			t.Fatal(err)
		}
	}

	walk := func(tok, view string, limit int) ([]string, int) {
		var out []string
		cursor := ""
		for page := 0; ; page++ {
			path := fmt.Sprintf("/api/v1/tickets?view=%s&limit=%d", view, limit)
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			res := f.do("GET", path, nil, bearer(tok))
			if res.Code != 200 {
				return nil, res.Code
			}
			for _, it := range res.Items() {
				out = append(out, refOf(it))
			}
			if cursor, _ = res.JSON()["nextCursor"].(string); cursor == "" {
				return out, 200
			}
		}
	}
	views := []string{"all", "myWork", "unassigned", "watching", "awaitingCustomer", "onHold", "dueSoon", "resolved", "archived"}
	for _, who := range []struct{ name, tok string }{{"admin", f.admin.Access}, {"marco", f.marco.Access},
		{"dana", f.dana.Access}, {"integration", f.integration}} {
		for _, view := range views {
			one, code := walk(who.tok, view, 100)
			if code != 200 {
				continue // a view this principal may not use
			}
			paged, _ := walk(who.tok, view, 7)
			if strings.Join(one, ",") != strings.Join(paged, ",") {
				t.Errorf("%s %s: paged %d tickets %v, one page %d tickets %v", who.name, view, len(paged), paged, len(one), one)
			}
			if who.name == "dana" && view == "all" {
				// Triage tickets, plus open/active/waiting leftovers assigned to her.
				want := 0
				for i := 0; i < n; i++ {
					workable := i%7 != 2 && i%7 != 3
					if i%10 == 0 || (i%5 == 2 && workable) {
						want++
					}
				}
				if len(one) != want {
					t.Errorf("dana all: %d tickets, want %d", len(one), want)
				}
			}
		}
	}

	// Plans: profile one walk of the All view per principal.
	db := col.Database()
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 2}}).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.RunCommand(context.Background(), bson.D{{Key: "profile", Value: 0}}).Err() })
	for _, who := range []struct{ name, tok string }{{"admin", f.admin.Access}, {"dana", f.dana.Access}, {"integration", f.integration}} {
		// Server time, not local: the test MongoDB may run on another host
		// whose clock differs, which would mix in the previous walk's queries.
		var hello struct {
			LocalTime time.Time `bson:"localTime"`
		}
		if err := db.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
			t.Fatal(err)
		}
		from := hello.LocalTime
		walk(who.tok, "all", 10)
		cur, err := db.Collection("system.profile").Find(ctx, bson.D{{Key: "ns", Value: db.Name() + ".tickets"},
			{Key: "command.find", Value: "tickets"}, {Key: "command.limit", Value: int64(11)}, {Key: "ts", Value: bson.D{{Key: "$gte", Value: from}}}})
		if err != nil {
			t.Fatal(err)
		}
		var prof []bson.M
		if err := cur.All(ctx, &prof); err != nil {
			t.Fatal(err)
		}
		var examined []int32
		for _, p := range prof {
			docs, _ := p["docsExamined"].(int32)
			examined = append(examined, docs)
			sorted, _ := p["hasSortStage"].(bool)
			plan, _ := p["planSummary"].(string)
			// Inside a group of tickets with the same update time the page
			// starts at the group's first ticket (the cursor's _id bound is a
			// filter), so the documents examined grow by one page per page
			// of that group; across distinct times a page reads about one
			// page of documents (TestReviewAllViewReadsInIndexOrder).
			if who.name != "dana" && (sorted || !strings.Contains(plan, "updatedAt: -1, _id: -1") && !strings.Contains(plan, "publicUpdatedAt: -1, _id: -1")) {
				t.Errorf("%s All view page: plan %s, in-memory sort %v, %d documents", who.name, plan, sorted, docs)
			}
			if who.name == "dana" {
				t.Logf("dana All view page: plan %s, in-memory sort %v, %d documents examined", plan, sorted, docs)
			}
		}
		t.Logf("%s: %d pages, documents examined per page %v", who.name, len(prof), examined)
	}
}
