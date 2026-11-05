package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

// Regression tests from the independent review of the ticket core (OC-TKT-*).

// waitAll runs fns concurrently and fails when they do not all finish within d.
func waitAll(t *testing.T, d time.Duration, fns ...func()) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, fn := range fns {
		wg.Go(func() {
			<-start
			fn()
		})
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("concurrent requests did not finish within %s", d)
	}
}

// OC-TKT-07: tag counts are computed only from tickets the caller may read.
// Tag names come from the admin-managed catalog, which every staff member
// sees; counts of another group's tickets never appear.
func TestReviewTagsRespectTicketAccess(t *testing.T) {
	f := newTicketFixture(t)
	mine := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	hidden := f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery}).Ref()
	f.addTags(f.admin, "dns", "legal-hold")
	f.do("PATCH", tpath(mine), map[string]any{"tags": []string{"dns"}}, bearer(f.dana.Access), ifMatchV(1)).expect(200)
	f.do("PATCH", tpath(hidden), map[string]any{"tags": []string{"dns", "legal-hold"}}, bearer(f.marco.Access), ifMatchV(1)).expect(200)
	f.get(f.dana.Access, hidden).expect(404, "ticketNotFound")

	tags := func(tok string) map[string]float64 {
		out := map[string]float64{}
		for _, tg := range f.do("GET", "/api/v1/tags", nil, bearer(tok)).expect(200).Items() {
			out[tg["tag"].(string)] = tg["count"].(float64)
		}
		return out
	}
	// Dana (Triage only) sees the catalog with her group's counts.
	if got := tags(f.dana.Access); len(got) != 2 || got["dns"] != 1 || got["legal-hold"] != 0 {
		t.Fatalf("agent sees tags of tickets she cannot read: %v", got)
	}
	// Marco (both groups) and the lead (all groups) see everything.
	for _, tok := range []string{f.marco.Access, f.lead.Access} {
		if got := tags(tok); len(got) != 2 || got["dns"] != 2 || got["legal-hold"] != 1 {
			t.Fatalf("full tag list: %v", got)
		}
	}
}

// rawContains reports whether any document of the collection contains needle
// anywhere (bodies are stored as strings or binary).
func (h *harness) rawContains(col, needle string) bool {
	h.t.Helper()
	cur, err := h.coll(col).Find(context.Background(), bson.D{})
	if err != nil {
		h.t.Fatal(err)
	}
	defer cur.Close(context.Background())
	for cur.Next(context.Background()) {
		if bytes.Contains(cur.Current, []byte(needle)) {
			return true
		}
	}
	return false
}

// OC-TKT-03 / OC-TKT-08: redaction also removes the copies of the text kept
// for idempotent replay (REST and MCP) and on the scheduled message that
// sent it; a late retry neither replays the old text nor posts a duplicate.
func TestReviewRedactionScrubsStoredCopies(t *testing.T) {
	f := newSLAFixture(t)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	redact := func(messageID string) {
		t.Helper()
		f.do("POST", tpath(key, "/messages/"+messageID+"/redact"), map[string]any{"reason": "contains a password"}, bearer(f.admin.Access)).expect(200)
	}

	// REST: a reply posted with an Idempotency-Key.
	rest := map[string]any{"visibility": "public", "body": "the password is hunter2-REST"}
	m := f.do("POST", tpath(key, "/messages"), rest, bearer(f.dana.Access), header("Idempotency-Key", "reply-1")).expect(201)
	if again := f.do("POST", tpath(key, "/messages"), rest, bearer(f.dana.Access), header("Idempotency-Key", "reply-1")).expect(201); again.Str("id") != m.Str("id") {
		t.Fatal("replay before redaction did not return the message")
	}
	redact(m.Str("id"))
	f.do("POST", tpath(key, "/messages"), rest, bearer(f.dana.Access), header("Idempotency-Key", "reply-1")).expect(409, "idempotencyKeyUsed")

	// MCP: a note added with an idempotencyKey.
	sess, err := connectMCP(t, srv.URL+"/mcp", f.dana.Access)
	if err != nil {
		t.Fatal(err)
	}
	note := map[string]any{"ticket": key, "audience": "agent", "body": "internal: hunter2-MCP", "idempotencyKey": "note-1"}
	out := mustCall(t, sess, "ticketsAddNote", note)
	redact(obj(out, "message")["id"].(string))
	mustFail(t, sess, "ticketsAddNote", note, "idempotencyKeyUsed")

	// A scheduled message that was sent keeps its text on the record.
	sm := f.schedule(f.dana.Access, key, map[string]any{"body": "root password: hunter2-SCHEDULED"}).expect(201).JSON()
	f.clk.Advance(time.Hour + time.Minute)
	f.runDue()
	sent := f.scheduledByID(key, sm["id"].(string))
	if sent["state"] != "sent" {
		t.Fatalf("scheduled message not sent: %v", sent)
	}
	redact(sent["sentMessageId"].(string))
	for _, tok := range []string{f.dana.Access, f.admin.Access} {
		list := f.do("GET", spath(key, "?state=all"), nil, bearer(tok)).expect(200)
		if strings.Contains(string(list.Body), "hunter2") || !strings.Contains(string(list.Body), "[redacted]") {
			t.Fatalf("scheduled message list after redaction: %s", list.Body)
		}
	}

	// Nothing was posted twice and no stored copy is left.
	if n := f.messageCount(key); n != 4 {
		t.Fatalf("%d messages, want 4 (a retry posted a duplicate)", n)
	}
	for _, col := range []string{"messages", "idempotencyRecords", "scheduledMessages"} {
		if f.rawContains(col, "hunter2") {
			t.Fatalf("%s still holds redacted text", col)
		}
	}
}

// OC-TKT-04: a follow-up of a ticket that already has 100 links is refused
// with 409 tooManyLinks (it answered 500).
func TestReviewFollowUpWithFullLinks(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	id := f.get(f.admin.Access, key).expect(200).Str("id")
	docs := make([]any, 0, tickets.MaxLinksPerTicket)
	for range tickets.MaxLinksPerTicket {
		docs = append(docs, bson.D{{Key: "_id", Value: bson.NewObjectID()}, {Key: "workspaceId", Value: bson.NewObjectID()},
			{Key: "projectId", Value: mustHex(t, f.projectID())}, {Key: "sourceTicketId", Value: bson.NewObjectID()},
			{Key: "targetTicketId", Value: mustHex(t, id)}, {Key: "type", Value: "relatedTo"},
			{Key: "createdBy", Value: bson.D{{Key: "type", Value: "user"}, {Key: "id", Value: f.dana.UserId}, {Key: "label", Value: ""}}},
			{Key: "createdAt", Value: time.Now()}})
	}
	if _, err := f.coll("ticketLinks").InsertMany(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	f.do("POST", tpath(key, "/follow-up"), map[string]any{}, bearer(f.dana.Access)).expect(409, "tooManyLinks")
	if n := len(f.do("GET", "/api/v1/tickets", nil, bearer(f.admin.Access)).expect(200).Items()); n != 1 {
		t.Fatalf("a refused follow-up created a ticket: %d tickets", n)
	}
}

// OC-TKT-09: a member removed from a group no longer reads (or replies to)
// the group's resolved and archived tickets that still name them as
// assignee; the release job returns their open work to the queue. The
// assignee of a ticket in a deactivated group keeps working it.
func TestReviewRemovedMemberLosesResolvedAssignedTickets(t *testing.T) {
	f := newNotifyFixture(t)
	resolved := f.createTicket(f.integration, map[string]any{"subject": "resolved"}).Ref()
	archived := f.createTicket(f.integration, map[string]any{"subject": "archived"}).Ref()
	open := f.createTicket(f.integration, map[string]any{"subject": "open"}).Ref()
	for _, key := range []string{resolved, archived, open} {
		f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(1)).expect(200)
	}
	f.do("POST", tpath(resolved, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(2)).expect(200)
	f.do("POST", tpath(archived, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(2)).expect(200)
	f.do("POST", tpath(archived, "/transitions"), map[string]any{"action": "archive"}, bearer(f.dana.Access), ifMatchV(3)).expect(200)
	f.do("POST", tpath(resolved, "/messages"), map[string]any{"visibility": "agent", "body": "internal detail"}, bearer(f.marco.Access)).expect(201)

	f.do("DELETE", "/api/v1/groups/"+f.triage+"/members/"+f.dana.UserId, nil, bearer(f.admin.Access)).expect(204)
	// Immediately: the resolved and archived tickets are gone for Dana.
	for _, key := range []string{resolved, archived} {
		f.get(f.dana.Access, key).expect(404, "ticketNotFound")
		f.do("GET", tpath(key, "/messages"), nil, bearer(f.dana.Access)).expect(404, "ticketNotFound")
		f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "still here"}, bearer(f.dana.Access)).expect(404, "ticketNotFound")
	}
	if ks := keys(f.do("GET", "/api/v1/tickets?view=resolved", nil, bearer(f.dana.Access)).expect(200).Items()); len(ks) != 0 {
		t.Fatalf("removed member lists resolved tickets: %v", ks)
	}
	// Her open ticket goes back to the queue through the release job.
	f.runDue()
	tk := f.get(f.admin.Access, open).expect(200).JSON()
	if tk["assignee"] != nil || obj(tk, "status")["name"] != "Open" {
		t.Fatalf("open ticket after removal: %v", tk)
	}
	f.get(f.dana.Access, open).expect(404, "ticketNotFound")
	// The historical assignee of the resolved ticket is kept for staff who can read it.
	if r := f.get(f.marco.Access, resolved).expect(200).JSON(); obj(r, "assignee")["id"] != f.dana.UserId {
		t.Fatalf("resolved ticket lost its assignee: %v", r)
	}

	// A deactivated group: the assignee keeps reading and working their ticket.
	work := f.createTicket(f.marco.Access, map[string]any{"subject": "delivery work", "groupId": f.delivery}).Ref()
	f.do("POST", tpath(work, "/claim"), nil, bearer(f.marco.Access), ifMatchV(1)).expect(200)
	f.do("PATCH", "/api/v1/groups/"+f.delivery, map[string]any{"active": false}, bearer(f.admin.Access), ifMatchV(1)).expect(200)
	f.get(f.marco.Access, work).expect(200)
	f.do("POST", tpath(work, "/messages"), map[string]any{"visibility": "public", "body": "Done."}, bearer(f.marco.Access)).expect(201)
	if ks := keys(f.do("GET", "/api/v1/tickets?view=myWork", nil, bearer(f.marco.Access)).expect(200).Items()); len(ks) != 1 || ks[0] != work {
		t.Fatalf("myWork in a deactivated group: %v", ks)
	}
}

// OC-TKT-02: a message page cursor carries no position, so an integration
// cannot read from it how many staff-only changes happened between two
// public messages; a cursor naming a hidden message is simply invalid.
func TestReviewMessageCursorHidesStaffActivity(t *testing.T) {
	f := newTicketFixture(t)
	decode := func(tok string) map[string]any {
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("cursor %q: %v", tok, err)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		return obj(m, "c")
	}
	cursorOf := func(notes int) (map[string]any, string, string) {
		key := f.createTicket(f.integration, map[string]any{}).Ref()
		f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "second"}, bearer(f.integration)).expect(201)
		noteID := ""
		for i := range notes {
			noteID = f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": fmt.Sprintf("note %d", i)}, bearer(f.dana.Access)).expect(201).Str("id")
		}
		f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "third"}, bearer(f.integration)).expect(201)
		f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "fourth"}, bearer(f.integration)).expect(201)
		p := f.do("GET", tpath(key, "/messages?limit=3"), nil, bearer(f.integration)).expect(200)
		return decode(p.Str("nextCursor")), key, noteID
	}
	plain, _, _ := cursorOf(0)
	noisy, key, noteID := cursorOf(3)
	if plain["s"] != noisy["s"] {
		t.Fatalf("cursor position differs with staff activity: %v vs %v", plain["s"], noisy["s"])
	}
	// Paging still follows commit order and skips nothing.
	var bodies []string
	cursor := ""
	for range 10 {
		r := f.do("GET", tpath(key, "/messages?limit=2&cursor="+cursor), nil, bearer(f.integration)).expect(200)
		for _, m := range r.Items() {
			bodies = append(bodies, m["body"].(string))
		}
		if r.JSON()["nextCursor"] == nil {
			break
		}
		cursor = r.Str("nextCursor")
	}
	if strings.Join(bodies[1:], ",") != "second,third,fourth" {
		t.Fatalf("paged bodies: %v", bodies)
	}
	// A cursor that names an agent note is invalid for the integration but
	// works for staff; a numeric legacy cursor carries no position.
	noteCursor := pagination.Cursor{Sort: "message", Id: noteID}.Encode()
	f.do("GET", tpath(key, "/messages?cursor="+noteCursor), nil, bearer(f.integration)).expect(422)
	if items := f.do("GET", tpath(key, "/messages?cursor="+noteCursor), nil, bearer(f.dana.Access)).expect(200).Items(); len(items) != 2 {
		t.Fatalf("staff paging after a note: %d items", len(items))
	}
	legacy := pagination.Cursor{Sort: "2", Id: noteID}.Encode()
	f.do("GET", tpath(key, "/messages?cursor="+legacy), nil, bearer(f.integration)).expect(422)
}

// Two simultaneous "watch" requests of the same user (double click, two tabs)
// must both answer quickly with the ticket watched exactly once.
func TestReviewConcurrentWatchIsIdempotent(t *testing.T) {
	f := newTicketFixture(t)
	for round := range 5 {
		key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
		res := make([]result, 4)
		fns := make([]func(), len(res))
		for i := range res {
			fns[i] = func() { res[i] = f.do("PUT", tpath(key, "/watchers/me"), nil, bearer(f.dana.Access)) }
		}
		waitAll(t, 20*time.Second, fns...)
		for i, r := range res {
			if r.Code != 200 || r.JSON()["watching"] != true {
				t.Fatalf("round %d request %d: %d %s", round, i, r.Code, r.Body)
			}
		}
		tk := f.get(f.dana.Access, key).expect(200).JSON()
		if tk["watcherCount"] != float64(1) || tk["watching"] != true {
			t.Fatalf("round %d: watcherCount=%v watching=%v", round, tk["watcherCount"], tk["watching"])
		}
	}
}
