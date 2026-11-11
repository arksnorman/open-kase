package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/mailer"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
)

// notifyFixture is a ticket fixture with SMTP configured against a fake
// mailer and fast retry timing.
type notifyFixture struct {
	*ticketFixture
	mail *mailer.Fake
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	f := &notifyFixture{ticketFixture: newTicketFixture(t), mail: &mailer.Fake{}}
	f.c.Mailers = func(mailer.Config) (mailer.Mailer, error) { return f.mail, nil }
	f.c.JobBackoff = func(int) time.Duration { return time.Millisecond }
	f.c.WebhookJitter = func(time.Duration) time.Duration { return 0 }
	f.updateSettings(func(s *settings.Settings) {
		s.Product.BaseURL = "https://support.example.com"
		s.SMTP = settings.SMTPSettings{Enabled: true, Host: "smtp.example.com", Port: 587, FromAddress: "support@example.com",
			FromName: "Support", TLSMode: "starttls"}
	})
	return f
}

func (f *ticketFixture) updateSettings(mutate func(*settings.Settings)) {
	f.t.Helper()
	snap := f.c.Settings.Current()
	if _, err := f.c.Settings.Update(context.Background(), settings.Update{Actor: audit.System(audit.SystemCLI, "test"), ExpectedVersion: snap.Version(),
		Mutate: mutate}); err != nil {
		f.t.Fatal(err)
	}
}

// drain dispatches every pending outbox event and runs every due job until
// nothing is left (retries use millisecond backoff in tests).
func (f *ticketFixture) drain() {
	f.t.Helper()
	ctx := context.Background()
	d := f.c.newDispatcher("test-dispatcher")
	r := f.c.newRunner("test-worker", 1)
	for i := 0; i < 2000; i++ {
		a, err := d.DispatchOnce(ctx)
		if err != nil {
			f.t.Fatalf("dispatch: %v", err)
		}
		b, err := r.RunOnce(ctx)
		if err != nil {
			f.t.Fatalf("run: %v", err)
		}
		if a || b {
			continue
		}
		queued, err := f.c.Jobs.List(ctx, jobs.Filter{State: jobs.StateQueued, Limit: 1})
		if err != nil {
			f.t.Fatal(err)
		}
		if len(queued) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("jobs did not drain")
}

// deliveries returns every delivery record, oldest first.
func (f *ticketFixture) deliveries() []notifications.Delivery {
	f.t.Helper()
	rows, err := f.c.NotifyRepo.ListDeliveries(context.Background(), notifications.DeliveryFilter{
		WorkspaceId: f.c.Settings.Current().WorkspaceID(), Limit: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

func (f *ticketFixture) deliveriesFor(eventType string) []notifications.Delivery {
	var out []notifications.Delivery
	for _, d := range f.deliveries() {
		if d.EventType == eventType {
			out = append(out, d)
		}
	}
	return out
}

func sentTo(m *mailer.Fake, email string) []mailer.Message {
	var out []mailer.Message
	for _, msg := range m.Sent() {
		if msg.To.Email == email {
			out = append(out, msg)
		}
	}
	return out
}

func (f *ticketFixture) reply(tok, key, visibility, body string) result {
	f.t.Helper()
	return f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": visibility, "body": body}, bearer(tok)).expect(201)
}

func (f *ticketFixture) watch(tok, key string) {
	f.t.Helper()
	f.do("PUT", tpath(key, "/watchers/me"), nil, bearer(tok)).expect(200)
}

func (f *ticketFixture) latestEvent(typ string) outbox.Record {
	f.t.Helper()
	var doc struct {
		Id bson.ObjectID `bson:"_id"`
	}
	if err := f.coll("outbox").FindOne(context.Background(), bson.D{{Key: "type", Value: typ}},
		optionsSortNewest()).Decode(&doc); err != nil {
		f.t.Fatal(err)
	}
	rec, err := f.c.NotifyRepo.GetEvent(context.Background(), doc.Id.Hex())
	if err != nil {
		f.t.Fatal(err)
	}
	return rec
}

func TestNotificationsEndToEnd(t *testing.T) {
	f := newNotifyFixture(t)
	// Integration creates a ticket for Priya: acknowledgement to Priya, queue
	// notice to the Triage members (Dana, Marco).
	f.createTicket(f.integration, map[string]any{})
	f.drain()
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 1 || !strings.Contains(got[0].Subject, "We received your request") ||
		strings.Contains(got[0].Text, "https://support.example.com") || !strings.Contains(got[0].Text, "not processed") {
		t.Fatalf("requester acknowledgement: %+v", got)
	}
	for _, who := range []string{"dana@example.com", "marco@example.com"} {
		got := sentTo(f.mail, who)
		if len(got) != 1 || !strings.Contains(got[0].Text, "https://support.example.com/tickets/1") {
			t.Fatalf("queue notice for %s: %+v", who, got)
		}
	}
	created := f.latestEvent(outbox.TicketCreated)
	if msgID := sentTo(f.mail, "dana@example.com")[0].MessageId; msgID != created.Id+".user-"+f.dana.UserId+"@example.com" {
		t.Fatalf("message id %q is not derived from the event", msgID)
	}

	// Marco watches; Dana replies publicly: Priya and Marco get the reply.
	f.watch(f.marco.Access, "1")
	f.mail.Reset()
	f.reply(f.dana.Access, "1", "public", "We are checking the MX records.")
	f.drain()
	// The requester is told that a response arrived, without its content.
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 1 || strings.Contains(got[0].Text+got[0].HTML, "checking the MX records") ||
		!strings.Contains(got[0].Text, "There is a new reply on ticket #1") {
		t.Fatalf("public reply to requester: %+v", got)
	}
	// Staff emails keep the reply text and the agent UI link.
	if got := sentTo(f.mail, "marco@example.com"); len(got) != 1 || !strings.Contains(got[0].Text, "checking the MX records") ||
		!strings.Contains(got[0].Text, "https://support.example.com/tickets/1") {
		t.Fatalf("public reply to watcher: %+v", got)
	}
	if got := sentTo(f.mail, "dana@example.com"); len(got) != 0 {
		t.Fatalf("author notified about own reply: %+v", got)
	}

	// Agent note: contacts never get it; watchers get a link-only notice.
	f.mail.Reset()
	f.reply(f.dana.Access, "1", "agent", "SECRET-NOTE customer is on the legacy cluster")
	f.drain()
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 0 {
		t.Fatalf("contact received an agent-note email: %+v", got)
	}
	for _, d := range f.deliveriesFor(outbox.MessageCreated) {
		if d.Recipient.Kind == notifications.RecipientContact && d.Template == "agentNoteNotice" {
			t.Fatalf("agent note delivery planned for a contact: %+v", d)
		}
	}
	got := sentTo(f.mail, "marco@example.com")
	if len(got) != 1 || strings.Contains(got[0].Text+got[0].HTML, "SECRET-NOTE") || !strings.Contains(got[0].Text, "internal note") {
		t.Fatalf("agent note notice must be link-only: %+v", got)
	}

	// Mute watcher emails: Marco's next watcher email is skipped (visible).
	f.do("PUT", "/api/v1/me/preferences", map[string]any{"emailPreferences": map[string]any{"watcherUpdates": false}},
		bearer(f.marco.Access)).expect(200)
	me := f.do("GET", "/api/v1/me", nil, bearer(f.marco.Access)).expect(200).JSON()
	if obj(me, "emailPreferences")["watcherUpdates"] != false || me["theme"] != "system" {
		t.Fatalf("preferences: %v", me)
	}
	f.mail.Reset()
	f.reply(f.dana.Access, "1", "public", "Second reply")
	f.drain()
	if got := sentTo(f.mail, "marco@example.com"); len(got) != 0 {
		t.Fatalf("muted watcher emailed: %+v", got)
	}
	muted := false
	for _, d := range f.deliveries() {
		if d.Recipient.Id == f.marco.UserId && d.Status == notifications.StatusSkipped && d.ErrorClass == "mutedByPreference" {
			muted = true
		}
	}
	if !muted {
		t.Fatal("muted delivery not recorded as skipped")
	}

	// Resolve: requester gets a status email without a link.
	v := f.version("1")
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(v)).expect(200)
	f.mail.Reset()
	f.drain()
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 1 || !strings.Contains(got[0].Subject, "resolved") || strings.Contains(got[0].Text, "https://") {
		t.Fatalf("resolve email: %+v", got)
	}
}

func TestOutboxFanOutIsIdempotentUnderConcurrency(t *testing.T) {
	f := newNotifyFixture(t)
	f.createTicket(f.integration, map[string]any{}) // #1, unassigned, Triage = Dana + Marco
	ev := f.latestEvent(outbox.TicketCreated)
	ctx := context.Background()

	// Four dispatchers race: two through the leased path, two calling
	// Dispatch directly on the same record (as a dispatcher whose lease
	// expired would).
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 2; i++ {
		d := f.c.newDispatcher("racer-" + strconv.Itoa(i))
		wg.Go(func() {
			if _, err := d.DispatchOnce(ctx); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			if err := d.Dispatch(ctx, ev); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("dispatch: %v", err)
	}
	// And once more after completion: nothing new.
	if err := f.c.newDispatcher("late").Dispatch(ctx, ev); err != nil {
		t.Fatal(err)
	}
	n, err := f.coll("jobs").CountDocuments(ctx, bson.D{{Key: "payload.eventId", Value: ev.Id}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // requester + Dana + Marco
		t.Fatalf("jobs for the event = %d, want 3 (one per recipient)", n)
	}
	nd, _ := f.coll("notificationDeliveries").CountDocuments(ctx, bson.D{{Key: "eventId", Value: mustHex(t, ev.Id)}})
	if nd != 3 {
		t.Fatalf("deliveries = %d, want 3", nd)
	}
	var out struct {
		State string `bson:"state"`
		Count int32  `bson:"fanoutCount"`
	}
	if err := f.coll("outbox").FindOne(ctx, bson.D{{Key: "_id", Value: mustHex(t, ev.Id)}}).Decode(&out); err != nil || out.State != "dispatched" || out.Count != 3 {
		t.Fatalf("outbox after fan-out: %+v %v", out, err)
	}
	// A dispatcher that crashed after creating jobs but before marking the
	// event leaves it pending; dispatching it again must not duplicate jobs
	// (unique logical keys), only finish the bookkeeping.
	if _, err := f.coll("outbox").UpdateOne(ctx, bson.D{{Key: "_id", Value: mustHex(t, ev.Id)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "state", Value: "pending"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.newDispatcher("after-crash").Dispatch(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.coll("jobs").CountDocuments(ctx, bson.D{{Key: "payload.eventId", Value: ev.Id}}); n != 3 {
		t.Fatalf("re-dispatch created duplicates: %d jobs", n)
	}
	f.drain()
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 1 {
		t.Fatalf("requester got %d acknowledgements, want exactly 1", len(got))
	}
}

func TestStaleWorkerCompletionIsRejected(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	store := f.c.Jobs
	if n, err := store.Enqueue(ctx, jobs.NewJob{Kind: "test.noop", Key: "noop:1"}, jobs.NewJob{Kind: "test.noop", Key: "noop:1"}); err != nil || n != 1 {
		t.Fatalf("enqueue with a duplicate key: n=%d err=%v", n, err)
	}
	now := time.Now().UTC()
	a, ok, err := store.Acquire(ctx, "worker-a", []string{"test.noop"}, now, 50*time.Millisecond)
	if err != nil || !ok || a.Generation != 1 || a.Attempts != 1 {
		t.Fatalf("acquire A: %+v %v %v", a, ok, err)
	}
	if _, ok, _ := store.Acquire(ctx, "worker-b", []string{"test.noop"}, now, time.Minute); ok {
		t.Fatal("a leased job was acquired twice")
	}
	// A's lease expires (A is paused or partitioned); B takes over.
	later := now.Add(time.Second)
	b, ok, err := store.Acquire(ctx, "worker-b", []string{"test.noop"}, later, time.Minute)
	if err != nil || !ok || b.Generation != 2 || b.Attempts != 2 {
		t.Fatalf("acquire B after expiry: %+v %v %v", b, ok, err)
	}
	leaseA := jobs.Lease{JobId: a.Id, Owner: "worker-a", Generation: a.Generation}
	leaseB := jobs.Lease{JobId: b.Id, Owner: "worker-b", Generation: b.Generation}
	wrote := false
	err = store.Complete(ctx, leaseA, jobs.Finish{State: jobs.StateSucceeded, Attempt: jobs.Attempt{Number: 1, Outcome: "succeeded"}},
		func(context.Context) error { wrote = true; return nil })
	if !errors.Is(err, jobs.ErrLeaseLost) || wrote {
		t.Fatalf("stale completion: err=%v, side write ran=%v", err, wrote)
	}
	if err := store.Fenced(ctx, leaseA, func(context.Context) error { return nil }); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale fenced write: %v", err)
	}
	if err := store.Extend(ctx, leaseA, later.Add(time.Hour)); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale extend: %v", err)
	}
	if err := store.Complete(ctx, leaseB, jobs.Finish{State: jobs.StateSucceeded, Attempt: jobs.Attempt{Number: 2, Generation: b.Generation, Outcome: "succeeded"}}, nil); err != nil {
		t.Fatalf("current completion: %v", err)
	}
	j, _ := store.Get(ctx, a.Id)
	if j.State != jobs.StateSucceeded || len(j.History) != 1 || j.History[0].Generation != 2 {
		t.Fatalf("job after completion: %+v", j)
	}
}

func TestWorkerCrashResumesAfterLeaseExpiry(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.createTicket(f.dana.Access, map[string]any{})
	if _, err := f.c.newDispatcher("d").DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Worker A leases every job, then "crashes" (never completes).
	var leased []jobs.Job
	for {
		j, ok, err := f.c.Jobs.Acquire(ctx, "crashed-worker", []string{notifications.KindEmail}, time.Now().UTC(), 200*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		leased = append(leased, j)
	}
	if len(leased) != 2 { // requester acknowledgement + Marco (queue); Dana created it
		t.Fatalf("leased %d jobs, want 2", len(leased))
	}
	// Before the lease expires nobody else runs them.
	r := f.c.newRunner("worker-b", 1)
	if worked, _ := r.RunOnce(ctx); worked {
		t.Fatal("a live lease was taken over")
	}
	time.Sleep(250 * time.Millisecond)
	f.drain()
	if len(f.mail.Sent()) != 2 {
		t.Fatalf("after resuming, sent %d emails, want 2", len(f.mail.Sent()))
	}
	for _, j := range leased {
		got, _ := f.c.Jobs.Get(ctx, j.Id)
		if got.State != jobs.StateSucceeded || got.Attempts != 2 || got.Generation != 2 {
			t.Fatalf("resumed job: %+v", got)
		}
	}
}

func TestRecipientLosingAccessIsSkipped(t *testing.T) {
	f := newNotifyFixture(t)
	f.createTicket(f.lead.Access, map[string]any{"groupId": f.delivery}) // #1 in Delivery (Marco only)
	f.watch(f.marco.Access, "1")
	f.watch(f.admin.Access, "1")
	f.drain()
	f.mail.Reset()
	f.reply(f.lead.Access, "1", "public", "Reply after which Marco loses access")
	ctx := context.Background()
	if _, err := f.c.newDispatcher("d").DispatchOnce(ctx); err != nil { // jobs planned for Priya, Marco, the admin
		t.Fatal(err)
	}
	// Between the event and the send, Marco is removed from the ticket's group.
	f.do("DELETE", "/api/v1/groups/"+f.delivery+"/members/"+f.marco.UserId, nil, bearer(f.admin.Access)).expect(204)
	f.drain()
	if got := sentTo(f.mail, "marco@example.com"); len(got) != 0 {
		t.Fatalf("recipient without access was emailed: %+v", got)
	}
	if len(sentTo(f.mail, adminEmail)) != 1 || len(sentTo(f.mail, "priya@acme.example")) != 1 {
		t.Fatalf("other recipients: %+v", f.mail.Sent())
	}
	found := false
	for _, d := range f.deliveriesFor(outbox.MessageCreated) {
		if d.Recipient.Id == f.marco.UserId {
			found = d.Status == notifications.StatusSkipped && d.ErrorClass == "recipientNoAccess"
		}
	}
	if !found {
		t.Fatalf("marco's delivery not skipped: %+v", f.deliveries())
	}
}

func TestSMTPFailureRetriesThenDeadLetters(t *testing.T) {
	f := newNotifyFixture(t)
	attempts := 0
	f.mail.FailWith = func(m mailer.Message) error {
		if m.To.Email == "priya@acme.example" {
			attempts++
			return &mailer.Error{Class: mailer.ClassTemporary, Phase: mailer.PhaseConnect, Err: errors.New("connection refused")}
		}
		return nil
	}
	f.createTicket(f.integration, map[string]any{})
	f.drain()
	if attempts != jobs.DefaultMaxAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, jobs.DefaultMaxAttempts)
	}
	var d notifications.Delivery
	for _, x := range f.deliveriesFor(outbox.TicketCreated) {
		if x.Recipient.Kind == notifications.RecipientContact {
			d = x
		}
	}
	if d.Status != notifications.StatusFailed || d.Attempts != jobs.DefaultMaxAttempts || d.ErrorClass != "smtpTemporary" {
		t.Fatalf("dead-lettered delivery: %+v", d)
	}
	j, _ := f.c.Jobs.Get(context.Background(), d.Id)
	if j.State != jobs.StateDead || len(j.History) != jobs.DefaultMaxAttempts {
		t.Fatalf("job: %+v", j)
	}
	// Visible to administrators, and retryable by hand.
	r := f.do("GET", "/api/v1/deliveries/"+d.Id, nil, bearer(f.admin.Access)).expect(200).JSON()
	if r["status"] != "failed" || r["attempts"] != float64(6) || r["recipient"].(map[string]any)["address"] != "priya@acme.example" {
		t.Fatalf("delivery view: %v", r)
	}
	f.do("GET", "/api/v1/deliveries/"+d.Id, nil, bearer(f.dana.Access)).expect(403)
	f.mail.FailWith = nil
	f.do("POST", "/api/v1/deliveries/"+d.Id+"/retry", nil, bearer(f.admin.Access)).expect(202)
	f.do("POST", "/api/v1/deliveries/"+d.Id+"/retry", nil, bearer(f.admin.Access)).expect(409, "deliveryNotRetryable")
	f.drain()
	if got := sentTo(f.mail, "priya@acme.example"); len(got) != 1 {
		t.Fatalf("manual retry sent %d", len(got))
	}
	if r := f.do("GET", "/api/v1/deliveries/"+d.Id, nil, bearer(f.admin.Access)).expect(200).JSON(); r["status"] != "sent" {
		t.Fatalf("after manual retry: %v", r)
	}

	// A permanent failure dead-letters at once; an unknown outcome is not
	// retried automatically.
	f.mail.FailWith = func(m mailer.Message) error {
		switch m.To.Email {
		case "dana@example.com":
			return &mailer.Error{Class: mailer.ClassPermanent, Phase: mailer.PhaseEnvelope, Code: 550, Err: errors.New("mailbox unavailable")}
		case "marco@example.com":
			return &mailer.Error{Class: mailer.ClassUnknownOutcome, Phase: mailer.PhaseCommit, Err: errors.New("connection reset after DATA")}
		}
		return nil
	}
	f.createTicket(f.integration, map[string]any{"subject": "Second"})
	f.drain()
	statuses := map[string]notifications.Delivery{}
	for _, x := range f.deliveriesFor(outbox.TicketCreated) {
		statuses[x.Recipient.Id] = x
	}
	if d := statuses[f.dana.UserId]; d.Status != notifications.StatusFailed || d.Attempts != 1 {
		t.Fatalf("permanent: %+v", d)
	}
	if d := statuses[f.marco.UserId]; d.Status != notifications.StatusUnknownOutcome || d.Attempts != 1 {
		t.Fatalf("unknown outcome: %+v", d)
	}
}

func TestSMTPNotConfiguredIsVisibleAsSkipped(t *testing.T) {
	f := newNotifyFixture(t)
	f.updateSettings(func(s *settings.Settings) { s.SMTP.Enabled = false })
	f.createTicket(f.integration, map[string]any{})
	f.drain()
	ds := f.deliveriesFor(outbox.TicketCreated)
	if len(ds) != 3 || len(f.mail.Sent()) != 0 {
		t.Fatalf("deliveries %d, sent %d", len(ds), len(f.mail.Sent()))
	}
	for _, d := range ds {
		if d.Status != notifications.StatusSkippedUnconfigured {
			t.Fatalf("delivery %+v, want skippedUnconfigured", d)
		}
	}
	r := f.do("GET", "/api/v1/deliveries?status=skippedUnconfigured", nil, bearer(f.admin.Access)).expect(200)
	if len(r.Items()) != 3 {
		t.Fatalf("list by status: %s", r.Body)
	}
}

func TestAssignmentsReleasedWhenAgentLosesGroup(t *testing.T) {
	f := newNotifyFixture(t)
	f.createTicket(f.dana.Access, map[string]any{"groupId": f.delivery}) // #1
	f.createTicket(f.dana.Access, map[string]any{})                      // #2 in Triage
	f.do("POST", tpath("1", "/claim"), nil, bearer(f.marco.Access), ifMatchV(f.version("1"))).expect(200)
	f.do("POST", tpath("2", "/claim"), nil, bearer(f.marco.Access), ifMatchV(f.version("2"))).expect(200)
	f.drain()

	// Removing Marco from Delivery releases #1 only (still in Triage).
	f.do("DELETE", "/api/v1/groups/"+f.delivery+"/members/"+f.marco.UserId, nil, bearer(f.admin.Access)).expect(204)
	f.drain()
	t1 := f.get(f.admin.Access, "1").expect(200).JSON()
	t2 := f.get(f.admin.Access, "2").expect(200).JSON()
	if t1["assignee"] != nil || obj(t1, "status")["category"] != "open" {
		t.Fatalf("#1 not released: %v", t1)
	}
	if obj(t2, "assignee")["id"] != f.marco.UserId {
		t.Fatalf("#2 should keep its eligible assignee: %v", t2)
	}
	ev, _ := f.c.Audit.RecentAudit(context.Background(), 50)
	released := false
	for _, e := range ev {
		if e.Action == "ticket.released" && e.ActorType == audit.ActorSystem {
			released = true
		}
	}
	if !released {
		t.Fatal("no system ticket.released audit event")
	}

	// Disabling Marco releases the rest.
	u := f.do("GET", "/api/v1/users/"+f.marco.UserId, nil, bearer(f.admin.Access)).expect(200)
	f.do("PATCH", "/api/v1/users/"+f.marco.UserId, map[string]any{"status": "disabled"}, bearer(f.admin.Access), ifMatchV(u.Version())).expect(200)
	f.drain()
	if t2 := f.get(f.admin.Access, "2").expect(200).JSON(); t2["assignee"] != nil {
		t.Fatalf("#2 not released after disable: %v", t2)
	}
}

// ---- Webhooks ----

type hookRequest struct {
	header http.Header
	body   []byte
}

type hookServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []hookRequest
	respond  func(n int, body []byte) int
}

func newHookServer(t *testing.T) *hookServer {
	hs := &hookServer{respond: func(int, []byte) int { return 204 }}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hs.mu.Lock()
		hs.requests = append(hs.requests, hookRequest{header: r.Header.Clone(), body: body})
		n := len(hs.requests)
		hs.mu.Unlock()
		w.WriteHeader(hs.respond(n, body))
	}))
	t.Cleanup(hs.Close)
	return hs
}

func (hs *hookServer) all() []hookRequest {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]hookRequest(nil), hs.requests...)
}

func TestWebhooksPublicOnlyAndRetried(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment // plain http to the local test receiver
	hs := newHookServer(t)
	path := "/api/v1/webhooks"
	body := map[string]any{"name": "Test hook", "url": hs.URL + "/hook", "eventTypes": []string{"ticket.created", "ticket.message.public", "ticket.statusChanged", "ticket.assigned"}}

	// Agent-note events need the explicit grant (installation admins only).
	f.do("POST", path, map[string]any{"name": "Test hook", "url": hs.URL, "eventTypes": []string{"ticket.message.agentNote"}}, bearer(f.admin.Access)).
		expect(422)
	f.do("GET", path, nil, bearer(f.dana.Access)).expect(403)

	created := f.do("POST", path, body, bearer(f.admin.Access)).expect(201).JSON()
	if created["secret"] != nil || created["secrets"] != nil || created["includeAgentNotes"] != false || obj(created, "auth")["type"] != "none" {
		t.Fatalf("created webhook: %v", created)
	}
	hookID := created["id"].(string)
	if n := len(f.do("GET", "/api/v1/integrations", nil, bearer(f.admin.Access)).expect(200).Items()); n != 2 {
		t.Fatalf("webhook principals must not appear as integrations (got %d)", n)
	}

	// The first delivery attempt of the reply event fails with 503.
	failedOnce := map[string]bool{}
	var mu sync.Mutex
	hs.respond = func(_ int, b []byte) int {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(string(b), "ticket.message.public") && !failedOnce["reply"] {
			failedOnce["reply"] = true
			return 503
		}
		return 204
	}
	f.createTicket(f.integration, map[string]any{})
	f.reply(f.dana.Access, "1", "public", "PUBLIC-REPLY visible to the requester")
	f.reply(f.dana.Access, "1", "agent", "AGENT-NOTE-BODY internal only")
	f.do("POST", tpath("1", "/claim"), nil, bearer(f.marco.Access), ifMatchV(f.version("1"))).expect(200)
	f.drain()

	reqs := hs.all()
	byType := map[string][]hookRequest{}
	for _, r := range reqs {
		if strings.Contains(string(r.body), "AGENT-NOTE-BODY") || strings.Contains(string(r.body), "agentNote") {
			t.Fatalf("agent note leaked to a public webhook: %s", r.body)
		}
		if strings.Contains(string(r.body), f.marco.UserId) || strings.Contains(string(r.body), "Marco") {
			t.Fatalf("staff assignee leaked to a public webhook: %s", r.body)
		}
		if r.header.Get("X-Open-Kase-Signature") != "" || r.header.Get("X-Open-Kase-Timestamp") != "" || r.header.Get("Authorization") != "" {
			t.Fatalf("unexpected headers: %v", r.header)
		}
		var env struct {
			Type    string `json:"type"`
			EventId string `json:"eventId"`
		}
		jsonUnmarshal(t, r.body, &env)
		if env.EventId != r.header.Get(webhooks.HeaderEventID) || env.Type != r.header.Get(webhooks.HeaderEventType) {
			t.Fatalf("event id header %q != body %q", r.header.Get(webhooks.HeaderEventID), env.EventId)
		}
		byType[env.Type] = append(byType[env.Type], r)
	}
	if len(byType["ticket.created"]) != 1 || len(byType["ticket.assigned"]) != 1 || len(byType["ticket.message.public"]) != 2 {
		t.Fatalf("deliveries by type: created=%d assigned=%d public=%d", len(byType["ticket.created"]), len(byType["ticket.assigned"]), len(byType["ticket.message.public"]))
	}
	// The retry kept the event identity and the exact bytes.
	first, second := byType["ticket.message.public"][0], byType["ticket.message.public"][1]
	if first.header.Get(webhooks.HeaderEventID) != second.header.Get(webhooks.HeaderEventID) || string(first.body) != string(second.body) {
		t.Fatalf("retry changed identity or bytes:\n%s\n%s", first.body, second.body)
	}
	if !strings.Contains(string(first.body), "PUBLIC-REPLY") || !strings.Contains(string(first.body), `"revision"`) {
		t.Fatalf("public payload: %s", first.body)
	}

	ds := f.do("GET", path+"/"+hookID+"/deliveries", nil, bearer(f.admin.Access)).expect(200).Items()
	if len(ds) != 3 { // created, public reply (2 attempts), assigned
		t.Fatalf("delivery history: %d", len(ds))
	}
	for _, d := range ds {
		if d["status"] != "sent" {
			t.Fatalf("delivery %v", d)
		}
		if d["eventType"] == "ticket.message.public" && (d["attempts"] != float64(2) || d["statusCode"] != float64(204)) {
			t.Fatalf("retried delivery %v", d)
		}
	}

	// Per-webhook stats (list and detail): 3 attempted, 2 delivered first try.
	for _, w := range []map[string]any{
		f.do("GET", path+"/"+hookID, nil, bearer(f.admin.Access)).expect(200).JSON(),
		f.do("GET", path, nil, bearer(f.admin.Access)).expect(200).Items()[0],
	} {
		st := obj(w, "stats")
		strip, _ := st["lastDeliveries"].([]any)
		if st["sentLast24h"] != float64(3) || st["retrying"] != float64(0) || len(strip) != 3 || st["lastSentAt"] == nil ||
			st["medianResponseMs"] == nil || st["pausedAt"] != nil {
			t.Fatalf("webhook stats: %v", st)
		}
		if r, _ := st["deliveredFirstTryRate"].(float64); r < 0.66 || r > 0.67 {
			t.Fatalf("first-try rate: %v", st["deliveredFirstTryRate"])
		}
		if strip[0].(map[string]any)["status"] != "sent" {
			t.Fatalf("strip: %v", strip)
		}
	}
	// Pausing records pausedAt; resuming clears it. No deliveries: null rate.
	paused := f.do("PATCH", path+"/"+hookID, map[string]any{"active": false}, bearer(f.admin.Access), ifMatchV(int64(created["version"].(float64)))).expect(200).JSON()
	if obj(paused, "stats")["pausedAt"] == nil {
		t.Fatalf("pausedAt not set: %v", paused)
	}
	resumed := f.do("PATCH", path+"/"+hookID, map[string]any{"active": true}, bearer(f.admin.Access), ifMatchV(int64(paused["version"].(float64)))).expect(200).JSON()
	if obj(resumed, "stats")["pausedAt"] != nil {
		t.Fatalf("pausedAt not cleared: %v", resumed)
	}
	empty := f.do("POST", path, map[string]any{"name": "Test hook", "url": hs.URL + "/other", "eventTypes": []string{"ticket.created"}}, bearer(f.admin.Access)).expect(201).JSON()
	if st := obj(f.do("GET", path+"/"+empty["id"].(string), nil, bearer(f.admin.Access)).expect(200).JSON(), "stats"); st["deliveredFirstTryRate"] != nil || st["sentLast24h"] != float64(0) {
		t.Fatalf("empty stats: %v", st)
	}
	f.do("DELETE", path+"/"+empty["id"].(string), nil, bearer(f.admin.Access)).expect(204)

	// Dead-letter (410 Gone) and manual retry with the same event ID.
	hs.respond = func(int, []byte) int { return 410 }
	v := f.version("1")
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(v)).expect(200)
	f.drain()
	var dead map[string]any
	for _, d := range f.do("GET", path+"/"+hookID+"/deliveries?status=failed", nil, bearer(f.admin.Access)).expect(200).Items() {
		dead = d
	}
	if dead == nil || dead["errorClass"] != "gone" {
		t.Fatalf("dead-lettered delivery missing: %v", dead)
	}
	before := len(hs.all())
	hs.respond = func(int, []byte) int { return 200 }
	f.do("POST", path+"/"+hookID+"/deliveries/"+dead["id"].(string)+"/retry", nil, bearer(f.admin.Access)).expect(202)
	f.drain()
	after := hs.all()
	if len(after) != before+1 || after[len(after)-1].header.Get(webhooks.HeaderEventID) != after[len(after)-2].header.Get(webhooks.HeaderEventID) {
		t.Fatalf("manual retry did not resend the same event (%d → %d)", before, len(after))
	}

	// The rotate route is gone.
	f.do("POST", path+"/"+hookID+"/rotate-secret", nil, bearer(f.admin.Access)).expect(404)
	f.do("POST", path+"/"+hookID+"/test", nil, bearer(f.admin.Access)).expect(202)
	f.drain()
	last := hs.all()[len(hs.all())-1]
	if !strings.Contains(string(last.body), `"webhook.test"`) || last.header.Get(webhooks.HeaderEventType) != "webhook.test" {
		t.Fatalf("test delivery body: %s", last.body)
	}
}

func TestWebhookAgentNotesOnlyWithAdminGrant(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	// Agents cannot manage webhooks, let alone grant notes.
	f.do("POST", "/api/v1/webhooks", map[string]any{"name": "Test hook", "url": hs.URL, "includeAgentNotes": true,
		"eventTypes": []string{"ticket.message.agentNote"}}, bearer(f.dana.Access)).expect(403)
	created := f.do("POST", "/api/v1/webhooks", map[string]any{"name": "Test hook", "url": hs.URL, "includeAgentNotes": true,
		"eventTypes": []string{"ticket.message.agentNote"}}, bearer(f.admin.Access)).expect(201).JSON()
	if created["includeAgentNotes"] != true {
		t.Fatalf("created: %v", created)
	}
	f.createTicket(f.dana.Access, map[string]any{})
	f.reply(f.dana.Access, "1", "public", "public text")
	f.reply(f.dana.Access, "1", "agent", "NOTE-FOR-STAFF-SYSTEM")
	f.drain()
	reqs := hs.all()
	if len(reqs) != 1 || !strings.Contains(string(reqs[0].body), "NOTE-FOR-STAFF-SYSTEM") || !strings.Contains(string(reqs[0].body), `"ticket.message.agentNote"`) {
		t.Fatalf("granted endpoint deliveries: %d %s", len(reqs), firstBody(reqs))
	}
}

func firstBody(r []hookRequest) string {
	if len(r) == 0 {
		return ""
	}
	return string(r[0].body)
}

// ---- SMTP settings and the test email over a real SMTP conversation ----

// fakeSMTP is a minimal plain-text SMTP server that records messages.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	w := func(l string) { _, _ = c.Write([]byte(l + "\r\n")) }
	w("220 fake ESMTP")
	var rcpt string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch strings.ToUpper(strings.SplitN(line, " ", 2)[0]) {
		case "EHLO", "HELO":
			w("250-fake")
			w("250 8BITMIME")
		case "MAIL":
			w("250 ok")
		case "RCPT":
			rcpt = line
			w("250 ok")
		case "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, rcpt+"\n"+b.String())
			s.mu.Unlock()
			w("250 queued")
		case "RSET", "NOOP":
			w("250 ok")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("502 unknown")
		}
	}
}

func (s *fakeSMTP) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func TestSMTPSettingsAndTestEmail(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Mailers = notifications.SMTPMailer // the real go-mail adapter
	srv := newFakeSMTP(t)
	port := srv.ln.Addr().(*net.TCPAddr).Port

	// Without configuration the test email is visibly skipped.
	f.updateSettings(func(s *settings.Settings) { s.SMTP.Enabled = false })
	d := f.do("POST", "/api/v1/settings/smtp/test", map[string]any{"to": "ops@example.com"}, bearer(f.admin.Access)).expect(202).JSON()
	f.drain()
	id := obj(d, "delivery")["id"].(string)
	if got := f.do("GET", "/api/v1/deliveries/"+id, nil, bearer(f.admin.Access)).expect(200).JSON(); got["status"] != "skippedUnconfigured" {
		t.Fatalf("unconfigured test email: %v", got)
	}

	cur := f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200)
	f.do("PUT", "/api/v1/settings/smtp", map[string]any{"enabled": true, "host": "127.0.0.1", "port": port, "username": "",
		"fromAddress": "support@example.com", "fromName": "Support", "tlsMode": "none"}, bearer(f.dana.Access), ifMatchV(int64(cur.JSON()["configVersion"].(float64)))).expect(403)
	v := int64(cur.JSON()["configVersion"].(float64))
	put := f.do("PUT", "/api/v1/settings/smtp", map[string]any{"enabled": true, "host": "127.0.0.1", "port": port, "username": "",
		"fromAddress": "support@example.com", "fromName": "Support", "tlsMode": "none"}, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON()
	if put["host"] != "127.0.0.1" || put["passwordConfigured"] != false || put["password"] != nil {
		t.Fatalf("smtp settings: %v", put)
	}
	// The rest of the settings is untouched by the SMTP-only update.
	if s := f.do("GET", "/api/v1/settings", nil, bearer(f.admin.Access)).expect(200).JSON(); obj(s, "product")["baseUrl"] != "https://support.example.com" {
		t.Fatalf("settings after smtp update: %v", s)
	}
	// The password is write-only.
	v = int64(put["configVersion"].(float64))
	pw := f.do("PUT", "/api/v1/settings/smtp", map[string]any{"enabled": true, "host": "127.0.0.1", "port": port, "username": "",
		"password": "", "fromAddress": "support@example.com", "fromName": "Support", "tlsMode": "none"}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	if strings.Contains(string(pw.Body), "password\":\"") {
		t.Fatalf("password echoed: %s", pw.Body)
	}

	d = f.do("POST", "/api/v1/settings/smtp/test", map[string]any{"to": "ops@example.com"}, bearer(f.admin.Access)).expect(202).JSON()
	id = obj(d, "delivery")["id"].(string)
	if obj(d, "delivery")["status"] != "pending" {
		t.Fatalf("test email delivery: %v", d)
	}
	f.drain()
	got := f.do("GET", "/api/v1/deliveries/"+id, nil, bearer(f.admin.Access)).expect(200).JSON()
	if got["status"] != "sent" || got["messageId"] == nil || obj(got, "recipient")["address"] != "ops@example.com" {
		t.Fatalf("test email delivery after send: %v", got)
	}
	msgs := srv.received()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "<ops@example.com>") || !strings.Contains(msgs[0], "Email delivery works") ||
		!strings.Contains(msgs[0], "Message-ID: <"+got["messageId"].(string)+">") {
		t.Fatalf("smtp server received: %q", msgs)
	}
	f.do("POST", "/api/v1/settings/smtp/test", map[string]any{"to": "not-an-email"}, bearer(f.admin.Access)).expect(422)
	f.do("POST", "/api/v1/settings/smtp/test", map[string]any{"to": "ops@example.com"}, bearer(f.dana.Access)).expect(403)
}

func optionsSortNewest() *options.FindOneOptionsBuilder {
	return options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}})
}

func jsonUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

func TestWebhookDestinations(t *testing.T) {
	f := newNotifyFixture(t) // APP_ENV=test: HTTPS only, no loopback
	path := "/api/v1/webhooks"
	hook := func(url string) map[string]any {
		return map[string]any{"name": "Test hook", "url": url, "eventTypes": []string{"ticket.created"}}
	}
	// Private networks are allowed without any setting; only admins
	// configure webhooks.
	for _, u := range []string{"https://192.168.1.10/hook", "https://10.0.0.5/hook", "https://[fd12::1]/hook", "https://100.64.0.9/hook"} {
		f.do("POST", path, hook(u), bearer(f.admin.Access)).expect(201)
	}
	for _, u := range []string{"https://127.0.0.1/hook", "https://localhost/hook", "https://169.254.169.254/latest",
		"https://169.254.1.1/hook", "https://0.0.0.0/hook", "http://192.168.1.10/hook"} {
		f.do("POST", path, hook(u), bearer(f.admin.Access)).expect(422)
	}
	// The removed allowlist setting is an unknown field.
	cur := f.do("GET", "/api/v1/settings", nil, bearer(f.admin.Access)).expect(200).JSON()
	if _, ok := cur["webhooks"]; ok {
		t.Fatalf("settings still expose webhooks: %v", cur)
	}
	smtp := cur["smtp"].(map[string]any)
	delete(smtp, "passwordConfigured")
	f.do("PUT", "/api/v1/settings", map[string]any{"product": cur["product"], "auth": cur["auth"], "files": cur["files"], "smtp": smtp,
		"webhooks": map[string]any{"allowedCidrs": []string{"10.0.0.0/8"}}}, bearer(f.admin.Access), ifMatchV(int64(cur["configVersion"].(float64)))).expect(422)
}
