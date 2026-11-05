package runtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/outbox"
)

// Regression tests from the asynchronous/delivery review (OC-ASYNC-*).

// OC-ASYNC-01 (second half): a payload stored for retries was built with
// the staff fields the grant allowed. When the grant is withdrawn (by an
// administrator, together with a new URL), the retry must
// rebuild the payload under the public projection.
func TestWebhookStoredPayloadIsRebuiltWhenGrantIsWithdrawn(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	approved, other := newHookServer(t), newHookServer(t)
	approved.respond = func(int, []byte) int { return 503 }
	pa := f.agent(f.admin, "pa@example.com", "Pat Admin", "admin")
	path := "/api/v1/webhooks"
	created := f.do("POST", path, map[string]any{"name": "Test hook", "url": approved.URL, "includeAgentNotes": true,
		"eventTypes": []string{"ticket.assigned"}}, bearer(f.admin.Access)).expect(201).JSON()
	id, v := created["id"].(string), int64(created["version"].(float64))

	f.createTicket(f.dana.Access, map[string]any{})
	f.do("POST", tpath("1", "/claim"), nil, bearer(f.marco.Access), ifMatchV(f.version("1"))).expect(200)
	// First attempt: the approved (granted) destination receives the
	// assignee and answers 503, so the delivery waits for its retry with
	// the payload stored.
	ctx := context.Background()
	d, r := f.c.newDispatcher("d"), f.c.newRunner("w", 1)
	for len(approved.all()) == 0 {
		if _, err := d.DispatchOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if first := string(approved.all()[0].body); !strings.Contains(first, f.marco.UserId) {
		t.Fatalf("granted payload lacks the assignee: %s", first)
	}

	// The project administrator withdraws the grant and re-points the
	// endpoint; the retry goes to the new URL.
	f.do("PATCH", path+"/"+id, map[string]any{"includeAgentNotes": false, "url": other.URL}, bearer(pa.Access), ifMatchV(v)).expect(200)
	f.drain()
	got := other.all()
	if len(got) == 0 {
		t.Fatal("the retry was not delivered to the new URL")
	}
	for _, req := range got {
		if strings.Contains(string(req.body), f.marco.UserId) || strings.Contains(string(req.body), "Marco") {
			t.Fatalf("staff identity sent to a destination without the grant: %s", req.body)
		}
		if !strings.Contains(string(req.body), `"ticket.assigned"`) {
			t.Fatalf("unexpected payload: %s", req.body)
		}
	}
}

// OC-ASYNC-05: redaction dropped the stored webhook payload of pending and
// retrying deliveries only. A dead-lettered delivery kept the old text and
// a manual retry re-sent it after the redaction; sent deliveries kept a
// copy of the redacted text at rest.
func TestRedactionDropsStoredPayloadsOfFailedAndSentDeliveries(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	down, up := newHookServer(t), newHookServer(t)
	down.respond = func(int, []byte) int { return 410 } // dead-letters at once
	path := "/api/v1/webhooks"
	failing := f.do("POST", path, map[string]any{"name": "Test hook", "url": down.URL, "eventTypes": []string{"ticket.message.public"}},
		bearer(f.admin.Access)).expect(201).Str("id")
	f.do("POST", path, map[string]any{"name": "Test hook", "url": up.URL, "eventTypes": []string{"ticket.message.public"}}, bearer(f.admin.Access)).expect(201)

	key := f.createTicket(f.integration, map[string]any{}).Ref()
	mid := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "my password is pineapple42"},
		bearer(f.integration)).expect(201).Str("id")
	f.drain()
	if len(down.all()) != 1 || len(up.all()) != 1 || !strings.Contains(string(down.all()[0].body), "pineapple42") {
		t.Fatalf("deliveries before the redaction: %d failing, %d delivered", len(down.all()), len(up.all()))
	}
	dead := f.do("GET", path+"/"+failing+"/deliveries?status=failed", nil, bearer(f.admin.Access)).expect(200).Items()
	if len(dead) != 1 {
		t.Fatalf("dead-lettered deliveries: %v", dead)
	}

	f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "Customer posted a password"}, bearer(f.admin.Access)).expect(200)

	// No copy of the redacted text stays in the delivery records ...
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
			t.Fatalf("a %s delivery still stores the redacted text", row.Status)
		}
	}
	// ... and a manual retry of the dead-lettered delivery sends the
	// redacted message, not the old text.
	down.respond = func(int, []byte) int { return 204 }
	f.do("POST", path+"/"+failing+"/deliveries/"+dead[0]["id"].(string)+"/retry", nil, bearer(f.admin.Access)).expect(202)
	f.drain()
	reqs := down.all()
	if len(reqs) != 2 {
		t.Fatalf("manual retry: %d requests", len(reqs))
	}
	if last := string(reqs[1].body); strings.Contains(last, "pineapple42") || !strings.Contains(last, "[redacted]") {
		t.Fatalf("manual retry after redaction sent: %s", last)
	}
}

// OC-ASYNC-02: display names are limited in code points (contact.name 200,
// user.displayName 100), but the mailer rejected names longer than 200
// bytes, so every email to a contact with a long non-ASCII name failed
// permanently.
func TestEmailToRecipientWithLongNonASCIIName(t *testing.T) {
	f := newNotifyFixture(t)
	name := strings.Repeat("Ж", 150) // 150 code points, 300 bytes
	f.createTicket(f.integration, map[string]any{"requester": map[string]any{"name": name, "email": "long@acme.example"}})
	f.drain()
	got := sentTo(f.mail, "long@acme.example")
	if len(got) != 1 {
		for _, d := range f.deliveriesFor(outbox.TicketCreated) {
			if d.Recipient.Kind == notifications.RecipientContact {
				t.Fatalf("acknowledgement not sent: status=%s class=%s error=%s", d.Status, d.ErrorClass, d.Error)
			}
		}
		t.Fatalf("acknowledgement not sent and no delivery recorded")
	}
	if got[0].To.Name != name {
		t.Fatalf("recipient name changed: %q", got[0].To.Name)
	}
}

// OC-ASYNC-03: when the runner dead-letters a job without the handler's
// commit (the last attempt's lease expired: worker killed or completion
// failed), the delivery record must not stay "pending"/"retrying" forever
// and must remain retryable by an administrator.
func TestDeliveryOfJobDeadLetteredAfterLeaseExpiryIsRetryable(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.createTicket(f.integration, map[string]any{}) // acknowledgement + Dana + Marco
	if _, err := f.c.newDispatcher("d").DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var d notifications.Delivery
	for _, x := range f.deliveriesFor(outbox.TicketCreated) {
		if x.Recipient.Kind == notifications.RecipientContact {
			d = x
		}
	}
	if d.Id == "" {
		t.Fatal("no requester delivery planned")
	}
	// Every attempt of this job is cut off (the worker is killed while it
	// holds the lease), until the attempt budget is spent.
	now := time.Now().UTC().Add(time.Millisecond)
	for i := 0; i < jobs.DefaultMaxAttempts; i++ {
		var got jobs.Job
		for {
			j, ok, err := f.c.Jobs.Acquire(ctx, "killed-worker", []string{notifications.KindEmail}, now, time.Millisecond)
			if err != nil || !ok {
				t.Fatalf("acquire %d: ok=%v err=%v", i, ok, err)
			}
			if j.Id == d.Id {
				got = j
				break
			}
			// Another recipient's job: complete it so only ours remains.
			if err := f.c.Jobs.Complete(ctx, jobs.Lease{JobId: j.Id, Owner: "killed-worker", Generation: j.Generation},
				jobs.Finish{State: jobs.StateSucceeded, Attempt: jobs.Attempt{Number: j.Attempts, Outcome: "succeeded"}}, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got.Attempts != i+1 {
			t.Fatalf("attempt %d recorded as %d", i+1, got.Attempts)
		}
		now = now.Add(2 * time.Millisecond)
	}
	// The next worker finds the budget spent and dead-letters the job.
	time.Sleep(30 * time.Millisecond)
	f.drain()
	j, err := f.c.Jobs.Get(ctx, d.Id)
	if err != nil || j.State != jobs.StateDead || j.LastClass != "attemptsExhausted" {
		t.Fatalf("job: %+v %v", j, err)
	}
	r := f.do("GET", "/api/v1/deliveries/"+d.Id, nil, bearer(f.admin.Access)).expect(200).JSON()
	if r["status"] != "failed" {
		t.Fatalf("delivery of a dead job is shown as %q, want failed", r["status"])
	}
	f.do("POST", "/api/v1/deliveries/"+d.Id+"/retry", nil, bearer(f.admin.Access)).expect(202)
	f.drain()
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 1 {
		t.Fatalf("manual retry sent %d emails, want 1", len(got))
	}
}

// Verification (no defect found): `serve` runs the HTTP server with
// ReadTimeout and WriteTimeout (60 s). A live update stream must outlive
// both; the handler extends the write deadline per write. The existing
// stream test used a server without timeouts, so this was unproven.
func TestLiveStreamOutlivesServerReadAndWriteTimeouts(t *testing.T) {
	f := newTicketFixture(t)
	f.c.LiveHeartbeat = 200 * time.Millisecond
	f.h = f.router(f.c)
	ts := httptest.NewUnstartedServer(f.h)
	ts.Config.ReadTimeout = time.Second // serve uses 60 s for both
	ts.Config.WriteTimeout = time.Second
	ts.Start()
	t.Cleanup(ts.Close)

	ch, stop := openStream(t, ts.URL, f.dana.Access)
	defer stop()
	if evs := collect(ch, 5*time.Second, func(e []sseEvent) bool { return len(e) > 0 }); len(evs) == 0 || evs[0].Event != "ready" {
		t.Fatalf("no ready event: %v", evs)
	}
	// Well past both timeouts the stream is still open and still delivers.
	deadline := time.After(2500 * time.Millisecond)
wait:
	for {
		select {
		case _, open := <-ch:
			if !open {
				t.Fatal("the stream was closed by the server's read/write timeout")
			}
		case <-deadline:
			break wait
		}
	}
	id := f.createTicket(f.dana.Access, map[string]any{}).Str("id")
	got := invalidations(collect(ch, 10*time.Second, func(e []sseEvent) bool { return len(invalidations(e)) >= 1 }))
	if len(got) != 1 || got[0] != "ticket.created@"+id {
		t.Fatalf("events after the timeouts: %v", got)
	}
}

// Verification: recipients are re-checked immediately before each send. A
// watcher disabled after dispatch, an agent removed from the ticket's group
// after dispatch (the ticket is then invisible to them) and a contact who is
// no longer the requester must not be emailed.
func TestRecipientsRecheckedAtSendTime(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.createTicket(f.integration, map[string]any{"groupId": f.delivery}) // #1 in Delivery (Marco only)
	f.watch(f.lead.Access, "1")
	f.drain()
	f.mail.Reset()
	f.reply(f.admin.Access, "1", "public", "Reply after which Lea is disabled and Marco leaves Delivery")
	if _, err := f.c.newDispatcher("d").DispatchOnce(ctx); err != nil { // Priya (requester), Lea (watcher) planned
		t.Fatal(err)
	}
	f.reply(f.integration, "1", "public", "Requester follow-up before the queue notice is sent")
	if _, err := f.c.newDispatcher("d").DispatchOnce(ctx); err != nil { // Marco (group queue), Lea (watcher) planned
		t.Fatal(err)
	}
	u := f.do("GET", "/api/v1/users/"+f.lead.UserId, nil, bearer(f.admin.Access)).expect(200)
	f.do("PATCH", "/api/v1/users/"+f.lead.UserId, map[string]any{"status": "disabled"}, bearer(f.admin.Access), ifMatchV(u.Version())).expect(200)
	f.do("DELETE", "/api/v1/groups/"+f.delivery+"/members/"+f.marco.UserId, nil, bearer(f.admin.Access)).expect(204)
	f.drain()
	if got := sentTo(f.mail, "lea@example.com"); len(got) != 0 {
		t.Fatalf("disabled watcher was emailed: %+v", got)
	}
	if got := sentTo(f.mail, "marco@example.com"); len(got) != 0 {
		t.Fatalf("agent who left the group was emailed: %+v", got)
	}
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 1 {
		t.Fatalf("requester emails: %d", len(got))
	}
	skipped := map[string]string{}
	for _, d := range f.deliveriesFor(outbox.MessageCreated) {
		if d.Status == notifications.StatusSkipped {
			skipped[d.Recipient.Id] = d.ErrorClass
		}
	}
	if skipped[f.lead.UserId] != "recipientInactive" || skipped[f.marco.UserId] != "recipientNoAccess" {
		t.Fatalf("skip reasons: %v", skipped)
	}
}

// Verification: a ticket moved out of an agent's group stops producing
// stream events for that agent, and an agent note never reaches an
// integration's stream.
func TestLiveStreamStopsAfterTicketLeavesAgentsGroup(t *testing.T) {
	f := newTicketFixture(t)
	f.c.LiveHeartbeat = 300 * time.Millisecond
	f.h = f.router(f.c)
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	dana, stopDana := openStream(t, ts.URL, f.dana.Access)
	defer stopDana()
	integ, stopInteg := openStream(t, ts.URL, f.readOnlyIntegraton)
	defer stopInteg()
	for _, ch := range []<-chan sseEvent{dana, integ} {
		if evs := collect(ch, 5*time.Second, func(e []sseEvent) bool { return len(e) > 0 }); len(evs) == 0 || evs[0].Event != "ready" {
			t.Fatalf("no ready event: %v", evs)
		}
	}
	key := f.createTicket(f.marco.Access, map[string]any{}).Ref() // Triage: Dana sees it
	id := f.get(f.admin.Access, key).Str("id")
	if got := invalidations(collect(dana, 10*time.Second, func(e []sseEvent) bool { return len(invalidations(e)) >= 1 })); len(got) != 1 {
		t.Fatalf("dana before the transfer: %v", got)
	}
	// Transfer to Delivery (Marco only); afterwards Dana must not learn
	// about activity on it, not even the agent note.
	f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery}, bearer(f.lead.Access), ifMatchV(f.version(key))).expect(200)
	f.reply(f.marco.Access, key, "agent", "internal note after the transfer")
	f.reply(f.marco.Access, key, "public", "public reply after the transfer")
	got := invalidations(collect(dana, 1500*time.Millisecond, func([]sseEvent) bool { return false }))
	for _, e := range got {
		if strings.HasSuffix(e, id) && !strings.HasPrefix(e, "ticket.assigned@") {
			t.Fatalf("Dana was told about a ticket she can no longer read: %v", got)
		}
	}
	// The integration sees the public changes only.
	evs := invalidations(collect(integ, 5*time.Second, func(e []sseEvent) bool { return len(invalidations(e)) >= 2 }))
	for _, e := range evs {
		if strings.HasPrefix(e, "ticket.assigned@") || strings.HasPrefix(e, "ticket.claimed@") {
			t.Fatalf("integration saw a staff-only change: %v", evs)
		}
	}
	if len(evs) != 2 || evs[0] != "ticket.created@"+id || evs[1] != "message.created@"+id {
		t.Fatalf("integration events: %v", evs)
	}
}

// OC-ASYNC-06: auto-resolution requires prior notice. When auto-resolution
// is enabled after a ticket's reminders were already sent (without the
// notice, because the policy had none), the ticket must not be resolved
// until the requester has received a reminder that says so.
func TestAutoResolveNeverHappensWithoutPriorNotice(t *testing.T) {
	f := newAutomationFixture(t)
	path := "/api/v1/automation"
	policy := map[string]any{"archiveAfterResolvedDays": 0, "customerFollowupAfterHours": 24, "maxReminders": 1,
		"reminderCooldownHours": 24, "autoResolveAfterHours": 0}
	f.do("PUT", path, policy, bearer(f.admin.Access), header("If-Match", "0")).expect(200)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.waiting(f.dana.Access, key)
	f.runDue()
	f.mail.Reset()
	f.advance(25 * time.Hour)
	f.runDue()
	if sent := sentTo(f.mail, "priya@acme.example"); f.reminders(key) != 1 || len(sent) != 1 || strings.Contains(sent[0].Text, "marked as resolved") {
		t.Fatalf("reminder without auto-resolution: %d reminders, %d emails", f.reminders(key), len(sent))
	}
	// The administrator now enables auto-resolution 48 h after the last
	// reminder; the only reminder so far carried no notice.
	policy["autoResolveAfterHours"] = 48
	f.do("PUT", path, policy, bearer(f.admin.Access), header("If-Match", "1")).expect(200)
	f.advance(50 * time.Hour)
	f.runDue()
	if f.category(key) != "waitingCustomer" {
		t.Fatalf("resolved without notice: %s", f.category(key))
	}
	sent := sentTo(f.mail, "priya@acme.example")
	if len(sent) != 2 || !strings.Contains(sent[1].Text, "marked as resolved") {
		t.Fatalf("expected one final reminder carrying the notice, got %d emails", len(sent))
	}
	// After the notice and the auto-resolve delay the ticket is resolved.
	f.advance(49 * time.Hour)
	f.runDue()
	if f.category(key) != "resolved" {
		t.Fatalf("not resolved after the notice: %s", f.category(key))
	}
	if n := f.reminders(key); n != 2 {
		t.Fatalf("%d reminders, want exactly 2 (no further nagging)", n)
	}
}

// OC-ASYNC-04: every automation scan schedules its successor, and the
// maintenance loop seeds one more scan per 15-minute window. The seeded
// scans must join the existing chain; otherwise each window adds another
// perpetual chain and the scan rate (and the jobs collection) grows without
// bound while the server runs.
func TestAutomationScanChainsDoNotMultiply(t *testing.T) {
	f := newAutomationFixture(t)
	ctx := context.Background()
	f.do("PUT", "/api/v1/automation", map[string]any{"archiveAfterResolvedDays": 3,
		"customerFollowupAfterHours": 24, "maxReminders": 2, "reminderCooldownHours": 24, "autoResolveAfterHours": 0},
		bearer(f.admin.Access), header("If-Match", "0")).expect(200)
	// Run due jobs one at a time; as in production, two jobs never run in
	// the same millisecond.
	r := f.c.newRunner("test-worker", 1)
	runDue := func() {
		t.Helper()
		for i := 0; i < 100; i++ {
			worked, err := r.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				return
			}
			f.clk.Advance(7 * time.Millisecond)
		}
		t.Fatal("scan jobs did not settle")
	}
	runDue()
	pending := func() int64 {
		n, err := f.coll("jobs").CountDocuments(ctx, bson.D{{Key: "kind", Value: automation.KindScan}, {Key: "state", Value: "queued"}})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := pending(); n != 1 {
		t.Fatalf("after enabling the policy %d scans are pending, want 1", n)
	}
	// Eight maintenance ticks (two hours of `serve`), each in a new window.
	for i := 0; i < 8; i++ {
		f.clk.Advance(automation.ScanInterval + time.Duration(i+1)*time.Second)
		if _, err := f.c.Automation.EnsureScheduled(ctx); err != nil {
			t.Fatal(err)
		}
		runDue()
		if n := pending(); n != 1 {
			t.Fatalf("after %d maintenance ticks %d scans are pending for one project, want 1", i+1, n)
		}
	}
	total, _ := f.coll("jobs").CountDocuments(ctx, bson.D{{Key: "kind", Value: automation.KindScan}})
	if total > 2*9+1 {
		t.Fatalf("%d scan jobs after 9 windows: scans are multiplying", total)
	}
}
