package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/mailer"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// statusIDs maps status categories (and "resolved"/"archived") to IDs.
func (f *ticketFixture) statusIDs() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, st := range f.do("GET", "/api/v1/statuses", nil, bearer(f.admin.Access)).expect(200).Items() {
		if _, ok := out[st["category"].(string)]; !ok {
			out[st["category"].(string)] = st["id"].(string)
		}
	}
	return out
}

func (f *ticketFixture) messageCount(key string) int {
	f.t.Helper()
	return len(f.do("GET", tpath(key, "/messages?limit=100"), nil, bearer(f.admin.Access)).expect(200).Items())
}

func category(r result) string { return obj(r.JSON(), "status")["category"].(string) }

func TestReplyWithStatusIsAtomic(t *testing.T) {
	f := newTicketFixture(t)
	st := f.statusIDs()
	key := f.createTicket(f.integration, map[string]any{"subject": "Reply and resolve"}).Ref()
	post := func(tok string, body map[string]any) result {
		return f.do("POST", tpath(key, "/messages"), body, bearer(tok))
	}

	// Invalid targets fail the whole request: nothing is posted.
	post(f.dana.Access, map[string]any{"visibility": "public", "body": "Archiving", "thenStatusId": st["archived"]}).expect(422)
	post(f.dana.Access, map[string]any{"visibility": "public", "body": "Unknown", "thenStatusId": "665f1c2b8e4a3d0012345678"}).expect(422)
	post(f.dana.Access, map[string]any{"visibility": "public", "body": "Bad", "thenStatusId": "nope"}).expect(422)
	// Integrations have no transition scope.
	post(f.integration, map[string]any{"visibility": "public", "body": "Close it", "thenStatusId": st["resolved"]}).expect(403, "insufficientScope")
	if n := f.messageCount(key); n != 1 {
		t.Fatalf("rejected requests posted messages: %d", n)
	}

	// An agent note may set a waiting status; public activity is unchanged.
	before := f.get(f.admin.Access, key).expect(200).JSON()
	r := post(f.dana.Access, map[string]any{"visibility": "agent", "body": "Asked the customer by phone", "thenStatusId": st["waitingCustomer"]}).expect(201)
	tk := obj(r.JSON(), "ticket")
	if obj(tk, "status")["category"] != "waitingCustomer" || tk["lastPublicMessageId"] != before["lastPublicMessageId"] {
		t.Fatalf("note + waiting: %v", tk)
	}

	// A public reply that resolves: message, status, audit and outbox commit together.
	r = post(f.dana.Access, map[string]any{"visibility": "public", "body": "Fixed: the MX record now points to us.", "thenStatusId": st["resolved"]}).expect(201)
	tk = obj(r.JSON(), "ticket")
	if obj(tk, "status")["category"] != "resolved" || tk["resolvedAt"] == nil || tk["lastPublicMessageId"] != r.Str("id") || tk["firstResponseAt"] == nil {
		t.Fatalf("reply + resolve: %v", tk)
	}
	acts := map[string]int{}
	for _, it := range f.do("GET", tpath(key, "/activity?limit=100"), nil, bearer(f.admin.Access)).expect(200).Items() {
		acts[it["action"].(string)]++
	}
	if acts["ticket.resolved"] != 1 || acts["ticket.statusChanged"] != 1 || acts["message.created"] != 2 {
		t.Fatalf("audit: %v", acts)
	}
	n, _ := f.coll("outbox").CountDocuments(context.Background(), bson.D{{Key: "type", Value: "ticket.statusChanged"}, {Key: "transition", Value: "resolve"}})
	if n != 1 {
		t.Fatalf("resolve outbox events: %d", n)
	}

	// Resolved: setting a waiting status needs a reopen first → 409, nothing posted.
	count := f.messageCount(key)
	post(f.dana.Access, map[string]any{"visibility": "public", "body": "One more thing", "thenStatusId": st["waitingCustomer"]}).expect(409, "invalidTransition")
	post(f.dana.Access, map[string]any{"visibility": "agent", "body": "Note", "thenStatusId": st["resolved"]}).expect(409, "invalidTransition")
	if f.messageCount(key) != count || category(f.get(f.admin.Access, key)) != "resolved" {
		t.Fatal("an invalid transition posted a message or changed the status")
	}
	// Without thenStatusId replies to resolved tickets keep working.
	post(f.dana.Access, map[string]any{"visibility": "agent", "body": "Plain note"}).expect(201)
}

// ---- scheduled messages ----

func spath(key string, rest ...string) string {
	return tpath(key, "/scheduled-messages"+strings.Join(rest, ""))
}

func (f *slaFixture) schedule(tok, key string, body map[string]any) result {
	f.t.Helper()
	if body["visibility"] == nil {
		body["visibility"] = "public"
	}
	if body["body"] == nil {
		body["body"] = "Good morning, the fix is deployed."
	}
	if body["sendAt"] == nil {
		body["sendAt"] = f.clk.Now().Add(time.Hour).Format(time.RFC3339)
	}
	return f.do("POST", spath(key), body, bearer(tok))
}

func (f *ticketFixture) scheduled(tok, key, state string) []map[string]any {
	f.t.Helper()
	return f.do("GET", spath(key, "?state="+state), nil, bearer(tok)).expect(200).Items()
}

func (f *ticketFixture) scheduledByID(key, id string) map[string]any {
	f.t.Helper()
	for _, it := range f.scheduled(f.admin.Access, key, "all") {
		if it["id"] == id {
			return it
		}
	}
	f.t.Fatalf("scheduled message %s not found", id)
	return nil
}

func TestScheduledMessageSendsAtTimeViaJob(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	st := f.statusIDs()
	key := f.createTicket(f.integration, map[string]any{"subject": "Schedule me", "priority": 1}).Ref()
	f.runDue()
	att := f.upload(f.dana.Access, "plan.txt", []byte("migration plan")).expect(201).Str("id")
	f.do("PUT", tpath(key, "/drafts/public"), map[string]any{"body": "draft text"}, bearer(f.dana.Access)).expect(200)
	ver := f.version(key)

	// Validation: past, far future, missing.
	f.schedule(f.dana.Access, key, map[string]any{"sendAt": f.clk.Now().Add(-time.Minute).Format(time.RFC3339)}).expect(422)
	f.schedule(f.dana.Access, key, map[string]any{"sendAt": f.clk.Now().Add(400 * 24 * time.Hour).Format(time.RFC3339)}).expect(422)
	f.schedule(f.dana.Access, key, map[string]any{"sendAt": "tomorrow"}).expect(422)
	f.schedule(f.integration, key, map[string]any{}).expect(403, "userRequired")

	// Send in 30 h (beyond the 24 h staging TTL of the attachment).
	sendAt := f.clk.Now().Add(30 * time.Hour)
	sm := f.schedule(f.dana.Access, key, map[string]any{"sendAt": sendAt.Format(time.RFC3339), "attachmentIds": []string{att},
		"thenStatusId": st["waitingCustomer"]}).expect(201).JSON()
	if sm["state"] != "scheduled" || sm["thenStatusId"] != st["waitingCustomer"] || len(sm["attachments"].([]any)) != 1 {
		t.Fatalf("scheduled: %v", sm)
	}
	// Nothing changed on the ticket; the draft was consumed.
	tk := f.get(f.admin.Access, key).expect(200).JSON()
	if int64(tk["version"].(float64)) != ver || tk["firstResponseAt"] != nil || f.messageCount(key) != 1 {
		t.Fatalf("scheduling changed the ticket: %v", tk)
	}
	if d := f.do("GET", tpath(key, "/drafts/public"), nil, bearer(f.dana.Access)).expect(200).JSON(); d["version"] != float64(0) {
		t.Fatalf("draft kept: %v", d)
	}
	// Private to the author (and project admins); never integrations.
	if len(f.scheduled(f.dana.Access, key, "active")) != 1 || len(f.scheduled(f.marco.Access, key, "active")) != 0 ||
		len(f.scheduled(f.admin.Access, key, "active")) != 1 {
		t.Fatal("scheduled list visibility")
	}
	f.do("GET", spath(key), nil, bearer(f.integration)).expect(403, "userRequired")
	if items := f.do("GET", tpath(key, "/messages"), nil, bearer(f.integration)).expect(200).Items(); len(items) != 1 {
		t.Fatalf("integration sees %d messages", len(items))
	}
	// The reserved upload cannot be attached elsewhere meanwhile.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "steal", "attachmentIds": []string{att}},
		bearer(f.dana.Access)).expect(422)

	// 25 h later the staging cleanup runs: the reserved upload survives.
	f.clk.Advance(25 * time.Hour)
	if _, err := f.c.Tickets.CleanupExpiredUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.coll("attachments").CountDocuments(context.Background(), bson.D{{Key: "_id", Value: mustHex(t, att)}}); n != 1 {
		t.Fatal("reserved attachment was cleaned up")
	}
	f.runDue()
	if f.messageCount(key) != 1 {
		t.Fatal("sent before sendAt")
	}
	// The SLA still counts the requester as waiting for a first response.
	if tk := f.get(f.admin.Access, key).expect(200).JSON(); tk["firstResponseAt"] != nil {
		t.Fatal("SLA saw the scheduled reply early")
	}

	// At sendAt the job posts it through the message command.
	f.clk.Advance(5 * time.Hour)
	f.runDue()
	msgs := f.do("GET", tpath(key, "/messages"), nil, bearer(f.integration)).expect(200).Items()
	if len(msgs) != 2 || msgs[1]["body"] != "Good morning, the fix is deployed." || obj(msgs[1], "author")["name"] != "Dana Okafor" ||
		len(msgs[1]["attachments"].([]any)) != 1 {
		t.Fatalf("sent messages: %v", msgs)
	}
	tk = f.get(f.admin.Access, key).expect(200).JSON()
	if obj(tk, "status")["category"] != "waitingCustomer" || ts(t, tk["firstResponseAt"]) != sendAt {
		t.Fatalf("after send: status %v firstResponseAt %v (want %v)", obj(tk, "status"), tk["firstResponseAt"], sendAt)
	}
	got := f.scheduledByID(key, sm["id"].(string))
	if got["state"] != "sent" || got["sentMessageId"] != msgs[1]["id"] || len(f.scheduled(f.dana.Access, key, "active")) != 0 {
		t.Fatalf("record after send: %v", got)
	}
	// Downloadable by readers of the message now.
	f.do("GET", "/api/v1/attachments/"+att+"/download", nil, bearer(f.integration)).expect(200)
	// Sending again is refused.
	f.do("POST", spath(key, "/", sm["id"].(string), "/send"), map[string]any{}, bearer(f.dana.Access)).expect(409, "scheduledMessageNotActive")
}

func TestScheduledMessageEditRescheduleCancel(t *testing.T) {
	f := newSLAFixture(t)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	att := f.upload(f.dana.Access, "a.txt", []byte("x")).expect(201).Str("id")
	sm := f.schedule(f.dana.Access, key, map[string]any{"attachmentIds": []string{att}}).expect(201).JSON()
	id := sm["id"].(string)
	v := int64(sm["version"].(float64))

	// Only the author edits; If-Match is required and checked.
	f.do("PATCH", spath(key, "/", id), map[string]any{"body": "Edited"}, bearer(f.marco.Access), ifMatchV(v)).expect(404, "scheduledMessageNotFound")
	f.do("PATCH", spath(key, "/", id), map[string]any{"body": "Edited"}, bearer(f.dana.Access)).expect(428)
	f.do("PATCH", spath(key, "/", id), map[string]any{"body": "Edited"}, bearer(f.dana.Access), ifMatchV(v+1)).expect(409, "versionConflict")
	later := f.clk.Now().Add(3 * time.Hour)
	up := f.do("PATCH", spath(key, "/", id), map[string]any{"body": "Edited", "sendAt": later.Format(time.RFC3339)},
		bearer(f.dana.Access), ifMatchV(v)).expect(200).JSON()
	if up["body"] != "Edited" || ts(t, up["sendAt"]) != later {
		t.Fatalf("update: %v", up)
	}
	// The old job (1 h) does nothing; the new one sends at 3 h.
	f.clk.Advance(time.Hour + time.Minute)
	f.runDue()
	if f.messageCount(key) != 1 {
		t.Fatal("stale job sent the message")
	}
	f.clk.Advance(2 * time.Hour)
	f.runDue()
	if f.messageCount(key) != 2 {
		t.Fatal("rescheduled message not sent")
	}

	// A project admin may cancel someone else's; the attachment returns to
	// the author's staging area with an expiry.
	att2 := f.upload(f.dana.Access, "b.txt", []byte("y")).expect(201).Str("id")
	sm2 := f.schedule(f.dana.Access, key, map[string]any{"attachmentIds": []string{att2}}).expect(201).JSON()
	f.do("POST", spath(key, "/", sm2["id"].(string), "/cancel"), nil, bearer(f.marco.Access)).expect(404)
	c := f.do("POST", spath(key, "/", sm2["id"].(string), "/cancel"), nil, bearer(f.admin.Access)).expect(200).JSON()
	if c["state"] != "cancelled" || c["reason"] != "cancelledByAdmin" {
		t.Fatalf("cancel: %v", c)
	}
	var a struct {
		ExpiresAt   *time.Time     `bson:"expiresAt"`
		ScheduledId *bson.ObjectID `bson:"scheduledMessageId"`
	}
	f.coll("attachments").FindOne(context.Background(), bson.D{{Key: "_id", Value: mustHex(t, att2)}}).Decode(&a)
	if a.ExpiresAt == nil || a.ScheduledId != nil {
		t.Fatalf("attachment not released: %+v", a)
	}
	f.clk.Advance(2 * time.Hour)
	f.runDue()
	if f.messageCount(key) != 2 {
		t.Fatal("cancelled message was sent")
	}
	// The released upload can be used in a normal message.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "with file", "attachmentIds": []string{att2}},
		bearer(f.dana.Access)).expect(201)
}

func TestScheduledMessageNotSentSilently(t *testing.T) {
	f := newSLAFixture(t)
	f.mail.Reset()
	// A: the requester replies after scheduling → needsReview, author emailed.
	a := f.createTicket(f.integration, map[string]any{"subject": "Needs review"}).Ref()
	smA := f.schedule(f.dana.Access, a, map[string]any{}).expect(201).JSON()
	// B: archived before send time → cancelled.
	b := f.createTicket(f.integration, map[string]any{"subject": "Archived"}).Ref()
	attB := f.upload(f.dana.Access, "b.txt", []byte("b")).expect(201).Str("id")
	smB := f.schedule(f.dana.Access, b, map[string]any{"attachmentIds": []string{attB}}).expect(201).JSON()
	// C: the author loses access to the ticket's group → cancelled.
	c := f.createTicket(f.integration, map[string]any{"subject": "Lost access"}).Ref()
	smC := f.schedule(f.marco.Access, c, map[string]any{}).expect(201).JSON()
	f.runDue()
	f.mail.Reset()

	f.do("POST", tpath(a, "/messages"), map[string]any{"visibility": "public", "body": "Actually it works again?"}, bearer(f.integration)).expect(201)
	f.do("POST", tpath(b, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(b))).expect(200)
	f.do("POST", tpath(b, "/transitions"), map[string]any{"action": "archive"}, bearer(f.dana.Access), ifMatchV(f.version(b))).expect(200)
	f.do("DELETE", "/api/v1/groups/"+f.triage+"/members/"+f.marco.UserId, nil, bearer(f.admin.Access)).expect(204)
	counts := map[string]int{a: f.messageCount(a), b: f.messageCount(b), c: f.messageCount(c)}

	f.clk.Advance(time.Hour + time.Minute)
	f.runDue()
	for k, n := range counts {
		if f.messageCount(k) != n {
			t.Fatalf("%s: message sent although it must not be", k)
		}
	}
	want := map[string][2]string{
		smA["id"].(string): {"needsReview", "newerRequesterReply"},
		smB["id"].(string): {"cancelled", "ticketArchived"},
		smC["id"].(string): {"cancelled", "authorNoAccess"},
	}
	for id, w := range want {
		var doc struct{ State, Reason string }
		f.coll("scheduledMessages").FindOne(context.Background(), bson.D{{Key: "_id", Value: mustHex(t, id)}}).Decode(&doc)
		if doc.State != w[0] || doc.Reason != w[1] {
			t.Fatalf("%s: %s/%s, want %v", id, doc.State, doc.Reason, w)
		}
	}
	if n, _ := f.coll("messages").CountDocuments(context.Background(), bson.D{{Key: "scheduledMessageId", Value: bson.D{{Key: "$exists", Value: true}}}}); n != 0 {
		t.Fatalf("%d scheduled deliveries", n)
	}
	// B's reserved attachment was released (it expires normally).
	var att struct {
		ExpiresAt *time.Time `bson:"expiresAt"`
	}
	f.coll("attachments").FindOne(context.Background(), bson.D{{Key: "_id", Value: mustHex(t, attB)}}).Decode(&att)
	if att.ExpiresAt == nil {
		t.Fatal("archived ticket kept the attachment reserved")
	}
	// The author was told by email, link only and without the body: A is held
	// for review, B was cancelled (archived). Nothing to Marco, who lost
	// access to C (review P-ASYNC-2: cancellations used to be silent).
	var review, cancelled []mailer.Message
	for _, m := range sentTo(f.mail, "dana@example.com") {
		switch {
		case strings.Contains(m.Subject, "was not sent"):
			review = append(review, m)
		case strings.Contains(m.Subject, "was cancelled"):
			cancelled = append(cancelled, m)
		}
	}
	if len(review) != 1 || strings.Contains(review[0].Text, "fix is deployed") || !strings.Contains(review[0].Text, "requester replied") {
		t.Fatalf("review notice: %+v", review)
	}
	if len(cancelled) != 1 || !strings.Contains(cancelled[0].Subject, b) || !strings.Contains(cancelled[0].Text, "archived") ||
		strings.Contains(cancelled[0].Text, "fix is deployed") || strings.Contains(cancelled[0].HTML, "fix is deployed") {
		t.Fatalf("cancellation notices: %+v", cancelled)
	}
	for _, m := range sentTo(f.mail, "marco@example.com") {
		if strings.Contains(m.Subject, "scheduled") {
			t.Fatalf("notice to the author who lost access: %s", m.Subject)
		}
	}
	for _, m := range f.mail.Sent() {
		if strings.Contains(m.Subject, "was not sent") && m.To.Email != "dana@example.com" {
			t.Fatalf("review notice to %s", m.To.Email)
		}
	}
	// The author reviews and sends it now (guarded by the newest public ID).
	stale := f.get(f.admin.Access, a).expect(200).JSON()["lastPublicMessageId"].(string)
	f.do("POST", spath(a, "/", smA["id"].(string), "/send"), map[string]any{"expectedLastPublicId": "665f1c2b8e4a3d0012345678"},
		bearer(f.dana.Access)).expect(409, "newerReplyExists")
	f.do("POST", spath(a, "/", smA["id"].(string), "/send"), map[string]any{}, bearer(f.marco.Access)).expect(404)
	sent2 := f.do("POST", spath(a, "/", smA["id"].(string), "/send"), map[string]any{"expectedLastPublicId": stale},
		bearer(f.dana.Access)).expect(201).JSON()
	if obj(sent2, "ticket")["lastPublicMessageId"] != sent2["id"] || f.scheduledByID(a, smA["id"].(string))["state"] != "sent" {
		t.Fatalf("send now: %v", sent2)
	}
	// Archived: the remaining record cannot be revived.
	f.do("PATCH", spath(b, "/", smB["id"].(string)), map[string]any{"body": "x", "sendAt": f.clk.Now().Add(time.Hour).Format(time.RFC3339)},
		bearer(f.dana.Access), ifMatchV(1)).expect(409, "ticketArchived")
}

// The author is told when an administrator or the system cancels their
// scheduled message, and not when they cancel it themselves (review P-ASYNC-2).
func TestScheduledCancellationEmailsTheAuthor(t *testing.T) {
	f := newSLAFixture(t)
	key := f.createTicket(f.integration, map[string]any{"subject": "Cancelled replies"}).Ref()
	byAdmin := f.schedule(f.dana.Access, key, map[string]any{}).expect(201).JSON()
	own := f.schedule(f.dana.Access, key, map[string]any{"visibility": "agent", "body": "Internal follow-up"}).expect(201).JSON()
	f.runDue()
	f.mail.Reset()

	f.do("POST", spath(key, "/", byAdmin["id"].(string), "/cancel"), nil, bearer(f.admin.Access)).expect(200)
	f.do("POST", spath(key, "/", own["id"].(string), "/cancel"), nil, bearer(f.dana.Access)).expect(200)
	f.runDue()

	var notices []mailer.Message
	for _, m := range f.mail.Sent() {
		if strings.Contains(m.Subject, "was cancelled") {
			notices = append(notices, m)
		}
	}
	if len(notices) != 1 || notices[0].To.Email != "dana@example.com" || !strings.Contains(notices[0].Subject, "public reply") ||
		!strings.Contains(notices[0].Text, "administrator cancelled it") || strings.Contains(notices[0].Text, "fix is deployed") {
		t.Fatalf("cancellation notices: %+v", notices)
	}
}

func TestScheduledSendJobFencing(t *testing.T) {
	f := newSLAFixture(t)
	ctx := context.Background()
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.runDue()
	sm := f.schedule(f.dana.Access, key, map[string]any{}).expect(201).JSON()
	f.clk.Advance(time.Hour)
	kinds := []string{tickets.KindScheduledSend}

	// Worker A takes the job, stalls past its lease; worker B takes over.
	ja, ok, err := f.c.Jobs.Acquire(ctx, "worker-a", kinds, f.clk.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire A: %v %v", ok, err)
	}
	leaseA := jobs.Lease{JobId: ja.Id, Owner: "worker-a", Generation: ja.Generation}
	resA := f.c.Tickets.HandleScheduledSend(ctx, ja, leaseA)
	jb, ok, err := f.c.Jobs.Acquire(ctx, "worker-b", kinds, f.clk.Now().Add(2*time.Minute), time.Minute)
	if err != nil || !ok || jb.Id != ja.Id || jb.Generation <= ja.Generation {
		t.Fatalf("takeover: %v %v %+v", ok, err, jb)
	}
	leaseB := jobs.Lease{JobId: jb.Id, Owner: "worker-b", Generation: jb.Generation}
	resB := f.c.Tickets.HandleScheduledSend(ctx, jb, leaseB)
	done := jobs.Finish{State: jobs.StateSucceeded, Attempt: jobs.Attempt{Outcome: "succeeded"}}
	if err := f.c.Jobs.Complete(ctx, leaseB, done, func(ctx context.Context) error { return resB.Commit(ctx, done) }); err != nil {
		t.Fatalf("complete B: %v", err)
	}
	// The stale worker cannot commit a second delivery.
	if err := f.c.Jobs.Complete(ctx, leaseA, done, func(ctx context.Context) error { return resA.Commit(ctx, done) }); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale completion: %v", err)
	}
	if n := f.messageCount(key); n != 2 {
		t.Fatalf("messages: %d", n)
	}
	// At-least-once safety: running the send again (a duplicate job) is a
	// no-op, and the unique index refuses a second delivery outright.
	if _, err := f.c.Jobs.Enqueue(ctx, jobs.NewJob{Kind: tickets.KindScheduledSend, Key: "dup:" + sm["id"].(string), RunAt: f.clk.Now(),
		WorkspaceId: f.c.Settings.Current().WorkspaceID(), ProjectId: f.projectID(),
		Payload: map[string]string{"projectId": f.projectID(), "scheduledId": sm["id"].(string), "revision": "1"}}); err != nil {
		t.Fatal(err)
	}
	f.runDue()
	if n := f.messageCount(key); n != 2 {
		t.Fatalf("duplicate job delivered again: %d", n)
	}
	var sent struct {
		WorkspaceId bson.ObjectID `bson:"workspaceId"`
		ProjectId   bson.ObjectID `bson:"projectId"`
		TicketId    bson.ObjectID `bson:"ticketId"`
	}
	f.coll("messages").FindOne(ctx, bson.D{{Key: "scheduledMessageId", Value: mustHex(t, sm["id"].(string))}}).Decode(&sent)
	_, err = f.coll("messages").InsertOne(ctx, bson.D{{Key: "_id", Value: bson.NewObjectID()}, {Key: "workspaceId", Value: sent.WorkspaceId},
		{Key: "projectId", Value: sent.ProjectId}, {Key: "ticketId", Value: sent.TicketId}, {Key: "visibility", Value: "public"},
		{Key: "author", Value: bson.D{{Key: "kind", Value: "user"}, {Key: "id", Value: bson.NewObjectID()}}}, {Key: "body", Value: "dup"},
		{Key: "attachmentCount", Value: int32(0)}, {Key: "seq", Value: int64(99)}, {Key: "createdAt", Value: f.clk.Now()},
		{Key: "scheduledMessageId", Value: mustHex(t, sm["id"].(string))}})
	if err == nil || !strings.Contains(err.Error(), "scheduledUnique") {
		t.Fatalf("second delivery accepted by the database: %v", err)
	}
}
