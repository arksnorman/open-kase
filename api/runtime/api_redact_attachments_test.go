package runtime

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// Owner-approved follow-up P-DATA-1: a redaction replaced the message body
// but kept its attachments, so a pasted password file or an ID scan stayed
// downloadable by everyone who can read the ticket (REST downloads, webhook
// metadata). The redact request's removeAttachments
// removes them in the redaction transaction; a durable job deletes the
// bytes after the commit.

// redactAttachmentFixture is a ticket created by the integration whose
// first (public) message carries one file.
type redactAttachmentFixture struct {
	*ticketFixture
	key, messageID, fileID string
}

func newRedactAttachmentFixture(f *ticketFixture, filename string) *redactAttachmentFixture {
	f.t.Helper()
	att := f.upload(f.integration, filename, []byte("\x89PNG\r\n\x1a\nscan of a passport")).expect(201).Str("id")
	key := f.createTicket(f.integration, map[string]any{"attachmentIds": []string{att}}).Ref()
	mid := f.do("GET", tpath(key, "/messages"), nil, bearer(f.admin.Access)).expect(200).Items()[0]["id"].(string)
	return &redactAttachmentFixture{ticketFixture: f, key: key, messageID: mid, fileID: att}
}

func (f *redactAttachmentFixture) redact(body map[string]any, opts ...reqOpt) result {
	f.t.Helper()
	return f.do("POST", tpath(f.key, "/messages/"+f.messageID+"/redact"), body, append([]reqOpt{bearer(f.admin.Access)}, opts...)...)
}

func (f *ticketFixture) attachmentDownload(tok, id string) result {
	f.t.Helper()
	return f.do("GET", "/api/v1/attachments/"+id+"/download", nil, bearer(tok))
}

// gridFSFile returns the GridFS file ID behind an attachment and how many
// files documents and chunks it still has.
func (f *ticketFixture) gridFSFile(attachmentID string) (bson.ObjectID, int64, int64) {
	f.t.Helper()
	ctx := context.Background()
	id, _ := bson.ObjectIDFromHex(attachmentID)
	var rec struct {
		FileId bson.ObjectID `bson:"fileId"`
	}
	if err := f.coll("attachments").FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&rec); err != nil {
		f.t.Fatalf("attachment %s: %v", attachmentID, err)
	}
	files, err := f.coll("attachmentBytes.files").CountDocuments(ctx, bson.D{{Key: "_id", Value: rec.FileId}})
	if err != nil {
		f.t.Fatal(err)
	}
	chunks, err := f.coll("attachmentBytes.chunks").CountDocuments(ctx, bson.D{{Key: "files_id", Value: rec.FileId}})
	if err != nil {
		f.t.Fatal(err)
	}
	return rec.FileId, files, chunks
}

// redactionAudits returns the changes of the message's message.redacted
// audit events, oldest first.
func (f *ticketFixture) redactionAudits(messageID string) [][]string {
	f.t.Helper()
	cur, err := f.coll("auditEvents").Find(context.Background(), bson.D{{Key: "action", Value: "message.redacted"},
		{Key: "target", Value: "message/" + messageID}})
	if err != nil {
		f.t.Fatal(err)
	}
	var docs []struct {
		Id      bson.ObjectID `bson:"_id"`
		Changes []string      `bson:"changes"`
	}
	if err := cur.All(context.Background(), &docs); err != nil {
		f.t.Fatal(err)
	}
	slices.SortFunc(docs, func(a, b struct {
		Id      bson.ObjectID `bson:"_id"`
		Changes []string      `bson:"changes"`
	}) int {
		return a.Id.Timestamp().Compare(b.Id.Timestamp())
	})
	out := [][]string{}
	for _, d := range docs {
		out = append(out, d.Changes)
	}
	return out
}

func placeholderOnly(t *testing.T, msg map[string]any, id string) {
	t.Helper()
	atts, _ := msg["attachments"].([]any)
	if len(atts) != 1 || !reflect.DeepEqual(atts[0], map[string]any{"id": id, "removed": true}) {
		t.Fatalf("attachments after removal: %v", msg["attachments"])
	}
}

func TestRedactionRemovesAttachments(t *testing.T) {
	f := newRedactAttachmentFixture(newTicketFixture(t), "passport-scan-AB1234567.png")
	ctx := context.Background()
	// Another message's file is not touched.
	other := f.upload(f.dana.Access, "zone-file.txt", []byte("acme-ltd.com. MX 10 mx1")).expect(201).Str("id")
	f.do("POST", tpath(f.key, "/messages"), map[string]any{"visibility": "agent", "body": "Zone attached.", "attachmentIds": []string{other}},
		bearer(f.dana.Access)).expect(201)
	f.attachmentDownload(f.integration, f.fileID).expect(200)
	f.attachmentDownload(f.dana.Access, f.fileID).expect(200)
	fileID, files, chunks := f.gridFSFile(f.fileID)
	if files != 1 || chunks == 0 {
		t.Fatalf("stored bytes before: %d files, %d chunks", files, chunks)
	}

	body := map[string]any{"reason": "ID scan posted by mistake", "removeAttachments": true}
	red := f.redact(body, header("Idempotency-Key", "redact-scan-1")).expect(200)
	placeholderOnly(t, red.JSON(), f.fileID)
	if strings.Contains(string(red.Body), "passport-scan") {
		t.Fatalf("the response names the removed file: %s", red.Body)
	}
	// A retry with the same Idempotency-Key replays the stored outcome.
	replay := f.redact(body, header("Idempotency-Key", "redact-scan-1")).expect(200)
	if replay.Header.Get("Idempotent-Replayed") != "true" || string(replay.Body) != string(red.Body) {
		t.Fatalf("replay: %v %s", replay.Header, replay.Body)
	}

	// No transport serves the file any more: 410 for those who could read
	// it, as before for everyone else.
	f.attachmentDownload(f.integration, f.fileID).expect(410, "attachmentRemoved")
	f.attachmentDownload(f.dana.Access, f.fileID).expect(410, "attachmentRemoved")
	f.attachmentDownload(f.admin.Access, f.fileID).expect(410, "attachmentRemoved")
	f.attachmentDownload(f.readOnlyIntegraton, f.fileID).expect(410, "attachmentRemoved")
	f.attachmentDownload(f.dana.Access, other).expect(200)
	for _, tok := range []string{f.admin.Access, f.integration} {
		page := f.do("GET", tpath(f.key, "/messages"), nil, bearer(tok)).expect(200)
		if strings.Contains(string(page.Body), "passport-scan") {
			t.Fatalf("the message list names the removed file: %s", page.Body)
		}
		placeholderOnly(t, page.Items()[0], f.fileID)
	}
	f.do("POST", tpath(f.key, "/assignments"), map[string]any{"groupId": f.delivery}, bearer(f.lead.Access), ifMatchV(f.version(f.key))).expect(200)
	f.attachmentDownload(f.dana.Access, f.fileID).expect(404, "attachmentNotFound")

	// At rest: no name, type, size or hash; who, when and why are recorded.
	oid, _ := bson.ObjectIDFromHex(f.fileID)
	type record struct {
		State       string `bson:"state"`
		Filename    string `bson:"filename"`
		ContentType string `bson:"contentType"`
		Size        int64  `bson:"size"`
		SHA256      string `bson:"sha256"`
		Removed     *struct {
			At time.Time `bson:"at"`
			By struct {
				Type string `bson:"type"`
				Id   string `bson:"id"`
			} `bson:"by"`
			Reason         string     `bson:"reason"`
			BytesDeletedAt *time.Time `bson:"bytesDeletedAt"`
		} `bson:"removed"`
	}
	var rec record
	if err := f.coll("attachments").FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&rec); err != nil {
		t.Fatal(err)
	}
	if rec.State != "removed" || rec.Filename != "" || rec.ContentType != "" || rec.Size != 0 || rec.SHA256 != "" || rec.Removed == nil ||
		rec.Removed.Reason != "ID scan posted by mistake" || rec.Removed.By.Type != "user" || rec.Removed.By.Id != f.admin.UserId ||
		rec.Removed.At.IsZero() || rec.Removed.BytesDeletedAt != nil {
		t.Fatalf("removed attachment record: %+v", rec)
	}
	// The audit event names the files by ID only.
	audits := f.redactionAudits(f.messageID)
	if len(audits) != 1 || !slices.Contains(audits[0], "body") || !slices.Contains(audits[0], "attachmentsRemoved:1") ||
		!slices.Contains(audits[0], "attachment/"+f.fileID) {
		t.Fatalf("redaction audit: %v", audits)
	}
	// The bytes are deleted after the commit by the job (the other
	// message's file stays).
	f.drain()
	if _, files, chunks := f.gridFSFile(f.fileID); files != 0 || chunks != 0 {
		t.Fatalf("bytes after the job: %d files, %d chunks of %s", files, chunks, fileID.Hex())
	}
	if _, files, _ := f.gridFSFile(other); files != 1 {
		t.Fatal("the other message's file was deleted")
	}
	rec = record{}
	if err := f.coll("attachments").FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&rec); err != nil {
		t.Fatal(err)
	}
	if rec.Removed == nil || rec.Removed.BytesDeletedAt == nil {
		t.Fatalf("bytes deletion not recorded: %+v", rec)
	}
	f.attachmentDownload(f.admin.Access, f.fileID).expect(410, "attachmentRemoved")
}

// Without removeAttachments nothing changes for the files (backward
// compatible); a second redaction with the flag removes the files kept the
// first time, and the stored response of the first request, which still
// names them, is no longer replayed.
func TestRedactionKeepsAttachmentsUnlessAskedAndRemovesThemLater(t *testing.T) {
	f := newRedactAttachmentFixture(newTicketFixture(t), "invoice-4711.pdf")
	first := f.redact(map[string]any{"reason": "Customer posted a password"}, header("Idempotency-Key", "redact-body")).expect(200).JSON()
	atts, _ := first["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["filename"] != "invoice-4711.pdf" || atts[0].(map[string]any)["removed"] != nil {
		t.Fatalf("attachments kept by a plain redaction: %v", first["attachments"])
	}
	f.attachmentDownload(f.integration, f.fileID).expect(200)
	f.attachmentDownload(f.dana.Access, f.fileID).expect(200)
	f.redact(map[string]any{"reason": "Customer posted a password"}).expect(409, "messageAlreadyRedacted")

	second := f.redact(map[string]any{"reason": "The invoice is private too", "removeAttachments": true}).expect(200).JSON()
	placeholderOnly(t, second, f.fileID)
	if second["body"] != "[redacted]" || second["redactedAt"] != first["redactedAt"] {
		t.Fatalf("the body redaction changed: %v then %v", first, second)
	}
	f.attachmentDownload(f.integration, f.fileID).expect(410, "attachmentRemoved")
	f.attachmentDownload(f.dana.Access, f.fileID).expect(410, "attachmentRemoved")
	f.redact(map[string]any{"reason": "Once more", "removeAttachments": true}).expect(409, "messageAlreadyRedacted")
	f.redact(map[string]any{"reason": "Customer posted a password"}, header("Idempotency-Key", "redact-body")).expect(409, "idempotencyKeyUsed")

	audits := f.redactionAudits(f.messageID)
	if len(audits) != 2 || slices.Contains(audits[0], "attachmentsRemoved:1") || slices.Contains(audits[1], "body") ||
		!slices.Contains(audits[1], "attachmentsRemoved:1") || !slices.Contains(audits[1], "attachment/"+f.fileID) {
		t.Fatalf("redaction audits: %v", audits)
	}
	f.drain()
	if _, files, chunks := f.gridFSFile(f.fileID); files != 0 || chunks != 0 {
		t.Fatalf("bytes after the job: %d files, %d chunks", files, chunks)
	}
}

// The webhook payload rebuilt after the removal (manual retry of a
// dead-lettered delivery) lists the placeholder, never the file name.
func TestRedactionRemovalRebuildsWebhookPayload(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	down := newHookServer(t)
	down.respond = func(int, []byte) int { return 410 } // dead-letters at once
	path := "/api/v1/webhooks"
	hook := f.do("POST", path, map[string]any{"name": "Test hook", "url": down.URL, "eventTypes": []string{"ticket.message.public"}}, bearer(f.admin.Access)).expect(201).Str("id")
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	att := f.upload(f.integration, "payroll-2026.xlsx", []byte("PK\x03\x04 salaries")).expect(201).Str("id")
	mid := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Payroll attached.", "attachmentIds": []string{att}},
		bearer(f.integration)).expect(201).Str("id")
	f.drain()
	if reqs := down.all(); len(reqs) != 1 || !strings.Contains(string(reqs[0].body), "payroll-2026.xlsx") {
		t.Fatalf("delivery before the removal: %d requests", len(reqs))
	}
	dead := f.do("GET", path+"/"+hook+"/deliveries?status=failed", nil, bearer(f.admin.Access)).expect(200).Items()
	if len(dead) != 1 {
		t.Fatalf("dead-lettered deliveries: %v", dead)
	}

	f.do("POST", tpath(key, "/messages/"+mid+"/redact"), map[string]any{"reason": "Salaries are private", "removeAttachments": true},
		bearer(f.admin.Access)).expect(200)
	down.respond = func(int, []byte) int { return 204 }
	f.do("POST", path+"/"+hook+"/deliveries/"+dead[0]["id"].(string)+"/retry", nil, bearer(f.admin.Access)).expect(202)
	f.drain()
	reqs := down.all()
	if len(reqs) != 2 {
		t.Fatalf("manual retry: %d requests", len(reqs))
	}
	last := string(reqs[1].body)
	if strings.Contains(last, "payroll-2026") || strings.Contains(last, "/attachments/") ||
		!strings.Contains(last, `"attachments":[{"id":"`+att+`","removed":true}]`) || !strings.Contains(last, "[redacted]") {
		t.Fatalf("payload rebuilt after the removal: %s", last)
	}
}

// The byte deletion survives a crash between the commit and the deletion
// (the job is committed with the redaction) and is retried after a failed
// attempt and after a worker that died holding the job.
func TestRemovedAttachmentBytesDeletionIsRetried(t *testing.T) {
	f := newRedactAttachmentFixture(newTicketFixture(t), "id-card.jpg")
	ctx := context.Background()
	f.redact(map[string]any{"reason": "ID card posted by mistake", "removeAttachments": true}).expect(200)

	// No worker ran yet (as after a crash right after the commit): the bytes
	// are still stored and the job waits in MongoDB.
	if _, files, chunks := f.gridFSFile(f.fileID); files != 1 || chunks == 0 {
		t.Fatalf("bytes before any worker: %d files, %d chunks", files, chunks)
	}
	queued, err := f.c.Jobs.List(ctx, jobs.Filter{Kind: tickets.KindDeleteRemovedFiles, Limit: 10})
	if err != nil || len(queued) != 1 || queued[0].State != jobs.StateQueued || queued[0].Payload["messageId"] != f.messageID {
		t.Fatalf("deletion job: %v %v", queued, err)
	}

	// A worker takes the job; its attempt fails (the storage is unreachable:
	// cancelled context) and asks for a retry, then the worker dies without
	// recording anything.
	j, ok, err := f.c.Jobs.Acquire(ctx, "crashed-worker", []string{tickets.KindDeleteRemovedFiles}, time.Now().UTC(), 5*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	failed, cancel := context.WithCancel(ctx)
	cancel()
	if res := f.c.Tickets.HandleDeleteRemovedFiles(failed, j, jobs.Lease{JobId: j.Id, Owner: "crashed-worker", Generation: j.Generation}); res.Outcome != jobs.Retry {
		t.Fatalf("failed attempt outcome: %+v", res)
	}
	if _, files, _ := f.gridFSFile(f.fileID); files != 1 {
		t.Fatal("bytes changed by the failed attempt")
	}
	time.Sleep(20 * time.Millisecond) // the dead worker's lease expires

	f.drain()
	if _, files, chunks := f.gridFSFile(f.fileID); files != 0 || chunks != 0 {
		t.Fatalf("bytes after the retry: %d files, %d chunks", files, chunks)
	}
	done, err := f.c.Jobs.Get(ctx, j.Id)
	if err != nil || done.State != jobs.StateSucceeded || done.Attempts != 2 {
		t.Fatalf("deletion job after the retry: %+v %v", done, err)
	}
	// Running it again is harmless.
	if res := f.c.Tickets.HandleDeleteRemovedFiles(ctx, done, jobs.Lease{}); res.Outcome != jobs.Done {
		t.Fatalf("repeated run: %+v", res)
	}
}
