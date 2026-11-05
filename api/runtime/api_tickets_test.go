package runtime

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

const agentPassword = "agent password 1"

type ticketFixture struct {
	*harness
	admin              session
	dana, marco, lead  session
	triage, delivery   string
	integration        string // token: tickets:read/write, messages:read/write
	readOnlyIntegraton string // requester token: tickets:read only (tickets and public messages)
}

// agent creates a user with role "agent" or "admin" in the given groups and
// signs them in.
func (h *harness) agent(admin session, email, name, role string, groups ...string) session {
	h.t.Helper()
	uid := h.createUser(admin, email, name)
	if role == "admin" {
		h.makeAdmin(admin, uid)
	}
	for _, g := range groups {
		h.do("PUT", "/api/v1/groups/"+g+"/members/"+uid, nil, bearer(admin.Access)).expect(200)
	}
	return h.login(email, agentPassword)
}

// addTags adds tags to the admin-managed catalog (only catalog tags can be
// applied to tickets).
func (h *harness) addTags(admin session, names ...string) {
	h.t.Helper()
	for _, n := range names {
		h.do("POST", "/api/v1/tags", map[string]any{"name": n}, bearer(admin.Access)).expect(201)
	}
}

func newTicketFixture(t *testing.T) *ticketFixture {
	t.Helper()
	return newTicketFixtureOn(newHarness(t))
}

func newTicketFixtureOn(h *harness) *ticketFixture {
	h.t.Helper()
	f := &ticketFixture{harness: h, admin: h.admin()}
	f.triage = h.project(f.admin).Str("defaultGroupId")
	f.delivery = h.do("POST", "/api/v1/groups", map[string]any{"name": "Delivery"}, bearer(f.admin.Access)).expect(201).Str("id")
	f.dana = h.agent(f.admin, "dana@example.com", "Dana Okafor", "agent", f.triage)
	f.marco = h.agent(f.admin, "marco@example.com", "Marco Silva", "agent", f.triage, f.delivery)
	f.lead = h.agent(f.admin, "lea@example.com", "Lea Lead", "admin")
	f.integration = h.do("POST", "/api/v1/integrations", map[string]any{"name": "Mail gateway",
		"scopes": []string{"tickets:read", "tickets:create", "tickets:replyAsRequester"}}, bearer(f.admin.Access)).expect(201).Str("token")
	f.readOnlyIntegraton = h.do("POST", "/api/v1/integrations", map[string]any{"name": "Reporting",
		"scopes": []string{"tickets:read"}}, bearer(f.admin.Access)).expect(201).Str("token")
	return f
}

func (f *ticketFixture) createTicket(tok string, body map[string]any) result {
	f.t.Helper()
	if body["requester"] == nil {
		body["requester"] = map[string]any{"name": "Priya Raman", "email": "priya@acme.example"}
	}
	if body["body"] == nil {
		body["body"] = "Inbound mail is bouncing with 550 5.1.1."
	}
	if body["subject"] == nil {
		body["subject"] = "Cannot receive external email"
	}
	return f.do("POST", "/api/v1/tickets", body, bearer(tok)).expect(201)
}

func tpath(key string, rest ...string) string {
	return "/api/v1/tickets/" + key + strings.Join(rest, "")
}

func (f *ticketFixture) get(tok, key string) result {
	f.t.Helper()
	return f.do("GET", tpath(key), nil, bearer(tok))
}

func (f *ticketFixture) version(key string) int64 {
	f.t.Helper()
	return f.get(f.admin.Access, key).expect(200).Version()
}

func obj(m map[string]any, key string) map[string]any { v, _ := m[key].(map[string]any); return v }

func keys(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, refOf(it))
	}
	return out
}

func (h *harness) upload(tok, filename string, content []byte) result {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(content)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, req)
	return result{t: h.t, Code: w.Code, Body: w.Body.Bytes(), Header: w.Header()}
}

func TestTicketCreateRoutingAndReads(t *testing.T) {
	f := newTicketFixture(t)
	tk := f.createTicket(f.dana.Access, map[string]any{}).JSON()
	if refOf(tk) != "1" || tk["number"] != float64(1) || tk["source"] != "web" || tk["assignee"] != nil || tk["priority"] != float64(3) {
		t.Fatalf("created ticket: %v", tk)
	}
	if obj(tk, "group")["name"] != "Triage" || obj(tk, "status")["name"] != "Open" || obj(tk, "status")["category"] != "open" ||
		obj(tk, "requestType")["name"] != "Issue" || obj(tk, "requester")["email"] != "priya@acme.example" || tk["lastPublicMessageId"] == nil {
		t.Fatalf("created ticket refs: %v", tk)
	}
	// By number, "#number" and ID; nothing else.
	f.get(f.dana.Access, "1").expect(200)
	f.get(f.dana.Access, tk["id"].(string)).expect(200)
	f.get(f.dana.Access, "99").expect(404, "ticketNotFound")
	f.get(f.dana.Access, "mail-1").expect(404, "ticketNotFound")
	f.get(f.dana.Access, "%231").expect(200)

	// Routed to an explicit active group; validation.
	d := f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery, "priority": 1, "subject": "SPF failing"}).JSON()
	if obj(d, "group")["name"] != "Delivery" || d["priority"] != float64(1) || refOf(d) != "2" {
		t.Fatalf("delivery ticket: %v", d)
	}
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "x", "body": "y", "priority": 0,
		"requester": map[string]any{"name": "A", "email": "a@b.example"}}, bearer(f.dana.Access)).expect(422)
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "x", "body": "y", "assigneeId": f.dana.UserId,
		"requester": map[string]any{"name": "A", "email": "a@b.example"}}, bearer(f.dana.Access)).expect(422) // not allowlisted
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "x", "body": "y"}, bearer(f.dana.Access)).expect(422)

	// Agents only see their groups' tickets; leads see all.
	if ks := keys(f.do("GET", "/api/v1/tickets", nil, bearer(f.dana.Access)).expect(200).Items()); len(ks) != 1 || ks[0] != "1" {
		t.Fatalf("dana sees %v", ks)
	}
	f.get(f.dana.Access, "2").expect(404, "ticketNotFound")
	if ks := keys(f.do("GET", "/api/v1/tickets?view=all", nil, bearer(f.lead.Access)).expect(200).Items()); len(ks) != 2 || ks[0] != "2" {
		t.Fatalf("lead sees %v", ks)
	}

	// Integration creates as the requester: requested priority recorded
	// separately, staff identities hidden, contact upserted by external ID.
	it := f.createTicket(f.integration, map[string]any{"priority": 1, "subject": "Password reset emails not arriving",
		"requester": map[string]any{"name": "Sam Ortiz", "email": "sam@globex.example", "externalId": "crm-42"}}).JSON()
	if it["source"] != "api" || it["priority"] != float64(3) || it["requestedPriority"] != float64(1) || obj(it, "requester")["externalId"] != "crm-42" {
		t.Fatalf("integration ticket: %v", it)
	}
	msgs := f.do("GET", tpath("3", "/messages"), nil, bearer(f.integration)).expect(200).Items()
	if len(msgs) != 1 || obj(msgs[0], "author")["context"] != "contact" || obj(msgs[0], "author")["name"] != "Sam Ortiz" {
		t.Fatalf("integration first message: %v", msgs)
	}
	// Integrations lacking tickets:write cannot create; contactId reuse.
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "x", "body": "y",
		"requester": map[string]any{"name": "A", "email": "a@b.example"}}, bearer(f.readOnlyIntegraton)).expect(403, "insufficientScope")
	c := f.createTicket(f.dana.Access, map[string]any{"requester": map[string]any{"contactId": obj(it, "requester")["id"]}}).JSON()
	if obj(c, "requester")["email"] != "sam@globex.example" {
		t.Fatalf("contactId requester: %v", c)
	}

	// Search by number and subject.
	if ks := keys(f.do("GET", "/api/v1/tickets?q=%233", nil, bearer(f.lead.Access)).expect(200).Items()); len(ks) != 1 || ks[0] != "3" {
		t.Fatalf("number search: %v", ks)
	}
	if ks := keys(f.do("GET", "/api/v1/tickets?q=SPF", nil, bearer(f.lead.Access)).expect(200).Items()); len(ks) != 1 || ks[0] != "2" {
		t.Fatalf("subject search: %v", ks)
	}
	// Pagination: stable cursor over 4 tickets.
	p1 := f.do("GET", "/api/v1/tickets?limit=3", nil, bearer(f.lead.Access)).expect(200)
	next := p1.Str("nextCursor")
	p2 := f.do("GET", "/api/v1/tickets?limit=3&cursor="+next, nil, bearer(f.lead.Access)).expect(200)
	if len(p1.Items()) != 3 || len(p2.Items()) != 1 || p2.JSON()["nextCursor"] != nil {
		t.Fatalf("pages: %d %d %v", len(p1.Items()), len(p2.Items()), p2.JSON()["nextCursor"])
	}
	// Audit and outbox written with the ticket.
	if !contains(f.auditActions(), "ticket.created:success") {
		t.Fatal("ticket.created not audited")
	}
	if n := f.outboxCount(t, "ticket.created"); n != 4 {
		t.Fatalf("outbox ticket.created = %d", n)
	}
}

func (f *ticketFixture) outboxCount(t *testing.T, typ string) int64 {
	t.Helper()
	n, err := f.coll("outbox").CountDocuments(context.Background(), bson.D{{Key: "type", Value: typ}})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConcurrentClaimsExactlyOneWins(t *testing.T) {
	f := newTicketFixture(t)
	agents := []session{f.dana, f.marco}
	for i := range 6 {
		agents = append(agents, f.agent(f.admin, fmt.Sprintf("agent%d@example.com", i), fmt.Sprintf("Agent %d", i), "agent", f.triage))
	}
	for round := range 5 {
		key := f.createTicket(f.dana.Access, map[string]any{"subject": fmt.Sprintf("Race %d", round)}).Ref()
		v := f.version(key)
		codes := make([]result, len(agents))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, a := range agents {
			wg.Go(func() {
				<-start
				codes[i] = f.do("POST", tpath(key, "/claim"), nil, bearer(a.Access), ifMatchV(v))
			})
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, r := range codes {
			switch {
			case r.Code == 200:
				wins++
			case r.Code == 409 && r.Problem().Code == "ticketAlreadyClaimed":
			default:
				t.Fatalf("round %d: unexpected %d %s", round, r.Code, r.Body)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d claims succeeded", round, wins)
		}
		tk := f.get(f.admin.Access, key).expect(200).JSON()
		if tk["assignee"] == nil || obj(tk, "status")["name"] != "Open" || obj(tk, "group")["name"] != "Triage" || tk["version"] != float64(v+1) {
			t.Fatalf("round %d: claimed ticket %v", round, tk)
		}
	}
	// Claims need group membership; leads use assignments instead.
	key := f.createTicket(f.dana.Access, map[string]any{"groupId": f.delivery}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.lead.Access), ifMatchV(1)).expect(403, "notGroupMember")
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.marco.Access)).expect(428)
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.integration), ifMatchV(1)).expect(403, "userRequired")
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.marco.Access), ifMatchV(1)).expect(200)
	if !contains(f.auditActions(), "ticket.claimed:success") {
		t.Fatal("claim not audited")
	}
	// Release requires a reason (ticket.releaseReason) and returns it to the queue.
	f.do("POST", tpath(key, "/release"), nil, bearer(f.marco.Access), ifMatchV(2)).expect(422)
	f.do("POST", tpath(key, "/release"), map[string]any{}, bearer(f.marco.Access), ifMatchV(2)).expect(422)
	f.do("POST", tpath(key, "/release"), map[string]any{"reason": "  x "}, bearer(f.marco.Access), ifMatchV(2)).expect(422)
	f.do("POST", tpath(key, "/release"), map[string]any{"reason": strings.Repeat("r", 501)}, bearer(f.marco.Access), ifMatchV(2)).expect(422)
	f.do("POST", tpath(key, "/release"), map[string]any{"reason": "Going on leave", "x": 1}, bearer(f.marco.Access), ifMatchV(2)).expect(422)
	f.do("POST", tpath(key, "/release"), map[string]any{"reason": "Going on leave"}, bearer(f.integration), ifMatchV(2)).expect(403, "userRequired")
	if f.version(key) != 2 {
		t.Fatal("refused release changed the ticket")
	}
	r := f.do("POST", tpath(key, "/release"), map[string]any{"reason": "Going on leave"}, bearer(f.marco.Access), ifMatchV(2)).expect(200).JSON()
	if r["assignee"] != nil || obj(r, "status")["name"] != "Open" {
		t.Fatalf("released: %v", r)
	}
	// The reason is audited and shown in the activity timeline.
	var found bool
	for _, a := range f.do("GET", tpath(key, "/activity"), nil, bearer(f.marco.Access)).expect(200).Items() {
		if a["action"] == "ticket.released" {
			found = a["reason"] == "Going on leave"
		}
	}
	if !found {
		t.Fatal("release reason missing from activity")
	}
}

func TestAgentNotesNeverReachIntegrations(t *testing.T) {
	f := newTicketFixture(t)
	tk := f.createTicket(f.integration, map[string]any{}).JSON()
	key := refOf(tk)
	before := f.get(f.integration, key).expect(200).JSON()

	time.Sleep(5 * time.Millisecond)
	note := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "MX still points to the old provider."},
		bearer(f.dana.Access)).expect(201).JSON()
	if note["visibility"] != "agent" || obj(note, "author")["context"] != "staff" {
		t.Fatalf("note: %v", note)
	}
	staff := f.do("GET", tpath(key, "/messages"), nil, bearer(f.dana.Access)).expect(200).Items()
	if len(staff) != 2 {
		t.Fatalf("staff sees %d messages", len(staff))
	}
	for _, path := range []string{tpath(key, "/messages"), tpath(key, "/messages?visibility=agent"), tpath(key, "/messages?visibility=public")} {
		for _, m := range f.do("GET", path, nil, bearer(f.integration)).expect(200).Items() {
			if m["visibility"] != "public" || strings.Contains(m["body"].(string), "MX") {
				t.Fatalf("integration received a private message via %s: %v", path, m)
			}
		}
	}
	// The note changed neither public activity nor the requester-side view.
	after := f.get(f.integration, key).expect(200).JSON()
	if after["activityAt"] != before["activityAt"] || after["lastPublicMessageId"] != before["lastPublicMessageId"] {
		t.Fatalf("agent note altered public activity: %v → %v", before["activityAt"], after["activityAt"])
	}
	if after["assignee"] != nil || after["watcherCount"] != float64(0) {
		t.Fatalf("integration view leaks staff data: %v", after)
	}
	staffView := f.get(f.dana.Access, key).expect(200).JSON()
	if staffView["activityAt"] == before["activityAt"] {
		t.Fatal("agent note did not update staff activity")
	}
	// Requester tokens can never write agent notes; tickets:read alone
	// reads the public messages only.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "x"}, bearer(f.integration)).expect(403, "insufficientScope")
	if page := f.do("GET", tpath(key, "/messages"), nil, bearer(f.readOnlyIntegraton)).expect(200); strings.Contains(string(page.Body), `"agent"`) {
		t.Fatalf("read-only token sees an agent note: %s", page.Body)
	}
	f.do("GET", tpath(key, "/drafts/public"), nil, bearer(f.integration)).expect(403, "userRequired")
	f.do("GET", "/api/v1/queue-counts", nil, bearer(f.integration)).expect(403, "userRequired")
	// Outbox records carry the audience, never bodies.
	var ev bson.M
	if err := f.coll("outbox").FindOne(context.Background(), bson.D{{Key: "type", Value: "message.created"}}).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	if ev["visibility"] != "agent" || ev["body"] != nil || ev["state"] != "pending" {
		t.Fatalf("outbox event: %v", ev)
	}
}

func TestRequesterReplyTransitions(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(1)).expect(200)
	statuses := f.do("GET", "/api/v1/statuses", nil, bearer(f.dana.Access)).expect(200).Items()
	var waiting string
	for _, s := range statuses {
		if s["category"] == "waitingCustomer" {
			waiting = s["id"].(string)
		}
	}
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "setStatus", "statusId": waiting, "reason": "asked for DNS details"},
		bearer(f.dana.Access), ifMatchV(2)).expect(200)
	// An agent note on a waiting ticket does not change its status.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "waiting"}, bearer(f.dana.Access)).expect(201)
	if c := obj(f.get(f.dana.Access, key).expect(200).JSON(), "status")["category"]; c != "waitingCustomer" {
		t.Fatalf("note changed status to %v", c)
	}
	r := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Here are the DNS details."},
		bearer(f.integration)).expect(201).JSON()
	if obj(r, "author")["context"] != "contact" || obj(obj(r, "ticket"), "status")["name"] != "Open" {
		t.Fatalf("requester reply: %v", r)
	}
	// Resolved + requester reply after the assignee left the group → Open, unassigned.
	v := f.version(key)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve", "reason": "fixed MX"}, bearer(f.dana.Access), ifMatchV(v)).expect(200)
	f.do("DELETE", "/api/v1/groups/"+f.triage+"/members/"+f.dana.UserId, nil, bearer(f.admin.Access)).expect(204)
	r = f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "It broke again."}, bearer(f.integration)).expect(201).JSON()
	tk := obj(r, "ticket")
	if obj(tk, "status")["name"] != "Open" {
		t.Fatalf("reply on resolved: %v", tk)
	}
	if staff := f.get(f.admin.Access, key).expect(200).JSON(); staff["assignee"] != nil || staff["resolvedAt"] != nil {
		t.Fatalf("ineligible assignee kept: %v", staff)
	}
	// Explicit transition rules.
	v = f.version(key)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive"}, bearer(f.marco.Access), ifMatchV(v)).expect(409, "invalidTransition")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "reopen"}, bearer(f.marco.Access), ifMatchV(v)).expect(409, "invalidTransition")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "fly"}, bearer(f.marco.Access), ifMatchV(v)).expect(422)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "setStatus", "statusId": obj(tk, "status")["id"], "x": 1},
		bearer(f.marco.Access), ifMatchV(v)).expect(422)
}

func TestArchivedTicketsAreFrozen(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.do("PUT", tpath(key, "/watchers/me"), nil, bearer(f.dana.Access)).expect(200)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(1)).expect(200)
	a := f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive"}, bearer(f.dana.Access), ifMatchV(2)).expect(200).JSON()
	if obj(a, "status")["category"] != "archived" || a["archivedAt"] == nil {
		t.Fatalf("archived: %v", a)
	}
	v := int64(3)
	frozen := []struct {
		method, path string
		body         any
		tok          string
	}{
		{"POST", "/messages", map[string]any{"visibility": "public", "body": "hello"}, f.dana.Access},
		{"POST", "/messages", map[string]any{"visibility": "agent", "body": "note"}, f.dana.Access},
		{"POST", "/messages", map[string]any{"visibility": "public", "body": "requester"}, f.integration},
		{"POST", "/transitions", map[string]any{"action": "reopen"}, f.dana.Access},
		{"POST", "/transitions", map[string]any{"action": "resolve"}, f.dana.Access},
		{"POST", "/assignments", map[string]any{"groupId": f.delivery}, f.lead.Access},
		{"POST", "/claim", nil, f.dana.Access},
		{"PATCH", "", map[string]any{"priority": 1}, f.dana.Access},
		{"PUT", "/drafts/public", map[string]any{"body": "draft"}, f.dana.Access},
		{"PUT", "/watchers/me", nil, f.marco.Access},
	}
	for _, c := range frozen {
		f.do(c.method, tpath(key, c.path), c.body, bearer(c.tok), ifMatchV(v)).expect(409, "ticketArchived")
	}
	// Unwatching stays allowed; reading too.
	if f.do("DELETE", tpath(key, "/watchers/me"), nil, bearer(f.dana.Access)).expect(200).JSON()["watching"] != false {
		t.Fatal("unwatch failed")
	}
	if got := f.version(key); got != v {
		t.Fatalf("archived ticket version moved to %d", got)
	}
}

func TestAttachmentsInheritMessageAudience(t *testing.T) {
	f := newTicketFixture(t)
	// The requester side uploads and creates a ticket with the file.
	up := f.upload(f.integration, `..\..\bounce-report.eml`, []byte("Received: from mx1\r\n\r\n550 5.1.1 recipient rejected")).expect(201).JSON()
	if up["filename"] != "bounce-report.eml" || up["scanStatus"] != "unscanned" || up["expiresAt"] == nil {
		t.Fatalf("upload: %v", up)
	}
	pubID := up["id"].(string)
	// Another principal cannot attach or download someone else's staged file.
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "attachmentIds": []string{pubID},
		"requester": map[string]any{"name": "A", "email": "a@b.example"}}, bearer(f.dana.Access)).expect(422)
	f.do("GET", "/api/v1/attachments/"+pubID+"/download", nil, bearer(f.dana.Access)).expect(404, "attachmentNotFound")
	key := f.createTicket(f.integration, map[string]any{"attachmentIds": []string{pubID}}).Ref()
	// A staged upload is consumed once.
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "s", "body": "b", "attachmentIds": []string{pubID},
		"requester": map[string]any{"name": "A", "email": "a@b.example"}}, bearer(f.integration)).expect(422)

	// Staff attach a file to an agent note.
	priv := f.upload(f.dana.Access, "zone-file.txt", []byte("acme-ltd.com. MX 10 mx1.old-provider.net.")).expect(201).Str("id")
	note := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "Old zone attached.", "attachmentIds": []string{priv}},
		bearer(f.dana.Access)).expect(201).JSON()
	if atts := note["attachments"].([]any); len(atts) != 1 {
		t.Fatalf("note attachments: %v", note)
	}

	dl := func(tok, id string) result {
		return f.do("GET", "/api/v1/attachments/"+id+"/download", nil, bearer(tok))
	}
	r := dl(f.integration, pubID).expect(200)
	if !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") || r.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(string(r.Body), "550 5.1.1") {
		t.Fatalf("public download: %v %q", r.Header, r.Body)
	}
	dl(f.dana.Access, pubID).expect(200)
	dl(f.dana.Access, priv).expect(200)
	// Agent-note files are invisible to integrations, as if missing.
	dl(f.integration, priv).expect(404, "attachmentNotFound")
	dl(f.readOnlyIntegraton, pubID).expect(200) // tickets:read covers public messages and their files
	// Staff without access to the ticket's group cannot download either.
	moved := f.version(key)
	f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery}, bearer(f.lead.Access), ifMatchV(moved)).expect(200)
	dl(f.dana.Access, pubID).expect(404, "attachmentNotFound")
	dl(f.dana.Access, priv).expect(404, "attachmentNotFound")
	dl(f.marco.Access, priv).expect(200)

	// Size limit (10 MiB default) and dangerous types served as octet-stream.
	f.upload(f.dana.Access, "big.bin", make([]byte, 10<<20+1)).expect(413, "attachmentTooLarge")
	html := f.upload(f.dana.Access, "x.html", []byte("<html><script>alert(1)</script></html>")).expect(201).Str("id")
	if ct := dl(f.dana.Access, html).expect(200).Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("html served as %q", ct)
	}
	f.upload(f.dana.Access, "empty.txt", nil).expect(422)
	if n, err := f.coll("attachments").CountDocuments(context.Background(), bson.D{{Key: "state", Value: "uploading"}}); err != nil || n != 0 {
		t.Fatalf("failed uploads left records: %d %v", n, err)
	}
}

func TestStagedUploadCleanup(t *testing.T) {
	f := newTicketFixture(t)
	id := f.upload(f.dana.Access, "a.txt", []byte("hello")).expect(201).Str("id")
	keep := f.upload(f.dana.Access, "b.txt", []byte("kept")).expect(201).Str("id")
	key := f.createTicket(f.dana.Access, map[string]any{"attachmentIds": []string{keep}}).Ref()
	repo := f.c.Tickets
	ctx := context.Background()
	// Nothing has expired yet.
	if n, err := repo.CleanupExpiredUploads(ctx); err != nil || n != 0 {
		t.Fatalf("cleanup before expiry: %d %v", n, err)
	}
	// Expire both records; only the unattached one is removed.
	col := f.coll("attachments")
	if _, err := col.UpdateOne(ctx, bson.D{{Key: "_id", Value: mustHex(t, id)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "expiresAt", Value: time.Now().Add(-time.Hour)}}}}); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.CleanupExpiredUploads(ctx); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	f.do("GET", "/api/v1/attachments/"+id+"/download", nil, bearer(f.dana.Access)).expect(404)
	f.do("GET", "/api/v1/attachments/"+keep+"/download", nil, bearer(f.dana.Access)).expect(200)
	if n, _ := f.coll("attachmentBytes.files").CountDocuments(ctx, bson.D{}); n != 1 {
		t.Fatalf("gridfs files left: %d", n)
	}
	_ = key
}

// coll opens a collection of the test database directly (assertions on
// stored state only).
func (h *harness) coll(name string) *mongo.Collection {
	h.t.Helper()
	u, err := url.Parse(h.uri)
	if err != nil {
		h.t.Fatal(err)
	}
	client, err := mongo.Connect(options.Client().ApplyURI(h.uri))
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client.Database(strings.TrimPrefix(u.Path, "/")).Collection(name)
}

func mustHex(t *testing.T, s string) bson.ObjectID {
	t.Helper()
	id, err := bson.ObjectIDFromHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Owner decision 2026-10-31: tickets are never deleted. The delete and
// restore routes and the trash view are gone; the router answers them as
// for any other unknown method or route, and the ticket is untouched.
func TestTicketDeletionRemoved(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	v := f.version(key)
	f.do("DELETE", tpath(key), map[string]any{"reason": "duplicate"}, bearer(f.admin.Access)).expect(405, "methodNotAllowed")
	f.do("POST", tpath(key, "/restore"), nil, bearer(f.admin.Access)).expect(404, "routeNotFound")
	f.do("GET", "/api/v1/tickets?view=deleted", nil, bearer(f.admin.Access)).expect(422)
	if f.get(f.admin.Access, key).expect(200).JSON()["deleted"] != nil || f.version(key) != v {
		t.Fatal("the ticket changed or still exposes deletion metadata")
	}
}

func TestTransferClearsIneligibleAssignee(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(1)).expect(200)

	// Agents transfer (owner decision 2026-10-31). Dana is not in Delivery:
	// transferring her own ticket there clears her and reopens the queue.
	r := f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery}, bearer(f.dana.Access), ifMatchV(2)).expect(200).JSON()
	if obj(r, "group")["name"] != "Delivery" || r["assignee"] != nil || obj(r, "status")["name"] != "Open" {
		t.Fatalf("transfer: %v", r)
	}
	if !contains(f.auditActions(), "ticket.transferred:success") {
		t.Fatal("transfer not audited")
	}
	// Explicit ineligible assignee is rejected; eligible one assigned.
	f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery, "assigneeId": f.dana.UserId},
		bearer(f.lead.Access), ifMatchV(3)).expect(422)
	r = f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery, "assigneeId": f.marco.UserId},
		bearer(f.lead.Access), ifMatchV(3)).expect(200).JSON()
	if obj(r, "assignee")["name"] != "Marco Silva" || obj(r, "status")["name"] != "Open" {
		t.Fatalf("assign: %v", r)
	}
	// Marco is in both groups: moving back to Triage keeps him.
	r = f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.triage}, bearer(f.lead.Access), ifMatchV(4)).expect(200).JSON()
	if obj(r, "assignee")["name"] != "Marco Silva" || obj(r, "group")["name"] != "Triage" {
		t.Fatalf("eligible kept: %v", r)
	}
	// Explicit null unassigns.
	r = f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.triage, "assigneeId": nil}, bearer(f.lead.Access), ifMatchV(5)).expect(200).JSON()
	if r["assignee"] != nil || obj(r, "status")["name"] != "Open" {
		t.Fatalf("unassign: %v", r)
	}
	// Inactive target group is refused.
	f.do("PATCH", "/api/v1/groups/"+f.delivery, map[string]any{"active": false}, bearer(f.admin.Access), ifMatchV(1)).expect(200)
	f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.delivery}, bearer(f.lead.Access), ifMatchV(6)).expect(422)
}

func TestTicketOptimisticVersions(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.do("PATCH", tpath(key), map[string]any{"priority": 2}, bearer(f.dana.Access)).expect(428, "preconditionRequired")
	r := f.do("PATCH", tpath(key), map[string]any{"priority": 2}, bearer(f.dana.Access), ifMatchV(1)).expect(200).JSON()
	if r["priority"] != float64(2) || r["version"] != float64(2) {
		t.Fatalf("patch: %v", r)
	}
	f.do("PATCH", tpath(key), map[string]any{"priority": 1}, bearer(f.marco.Access), ifMatchV(1)).expect(409, "versionConflict")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.marco.Access), ifMatchV(1)).expect(409, "versionConflict")
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.marco.Access), ifMatchV(1)).expect(409, "versionConflict")
	f.do("PATCH", tpath(key), map[string]any{"statusId": "x"}, bearer(f.dana.Access), ifMatchV(2)).expect(422)
	f.do("PATCH", tpath(key), map[string]any{"priority": 5}, bearer(f.dana.Access), ifMatchV(2)).expect(422)
	f.do("PATCH", tpath(key), map[string]any{"priority": 1}, bearer(f.integration), ifMatchV(2)).expect(403, "userRequired")
	// Unchanged PATCH does not bump the version.
	if f.do("PATCH", tpath(key), map[string]any{"priority": 2}, bearer(f.dana.Access), ifMatchV(2)).expect(200).Version() != 2 {
		t.Fatal("no-op patch changed version")
	}
}

func TestTicketCreateIdempotency(t *testing.T) {
	f := newTicketFixture(t)
	body := map[string]any{"subject": "Shared calendar", "body": "Please set up a shared calendar.",
		"requester": map[string]any{"name": "Priya Raman", "email": "priya@acme.example"}}
	first := f.do("POST", "/api/v1/tickets", body, bearer(f.integration), header("Idempotency-Key", "crm-req-1")).expect(201)
	replay := f.do("POST", "/api/v1/tickets", body, bearer(f.integration), header("Idempotency-Key", "crm-req-1")).expect(201)
	if replay.Header.Get("Idempotent-Replayed") != "true" || replay.Str("id") != first.Str("id") {
		t.Fatalf("replay: %s", replay.Body)
	}
	body["subject"] = "Different"
	f.do("POST", "/api/v1/tickets", body, bearer(f.integration), header("Idempotency-Key", "crm-req-1")).expect(409, "idempotencyKeyReused")

	// Crash after commit, before the response was stored: the marker
	// written in the ticket transaction makes the retry return the same
	// ticket instead of creating a duplicate.
	ctx := context.Background()
	ref := idempotency.Ref{Scope: "user:" + f.dana.UserId + ":crash-test", Key: "k1"}
	if _, err := f.c.Idempotency.Begin(ctx, ref.Scope, ref.Key, strings.Repeat("a", 64), time.Now()); err != nil {
		t.Fatal(err)
	}
	scope, err := f.c.Access.Scope(ctx, f.principal(t, f.dana))
	if err != nil {
		t.Fatal(err)
	}
	in := tickets.CreateInput{Subject: "Crash", Body: "b", Requester: tickets.RequesterInput{Name: strp("A"), Email: strp("a@b.example")}}
	t1, err := f.c.Tickets.Create(idempotency.WithRef(ctx, ref), scope, in)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := f.c.Tickets.Create(idempotency.WithRef(ctx, ref), scope, in)
	if err != nil || t2.Id != t1.Id {
		t.Fatalf("retry created %v (%v), want %v", tnum(t2), err, tnum(t1))
	}
	n, _ := f.coll("tickets").CountDocuments(ctx, bson.D{{Key: "subject", Value: "Crash"}})
	if n != 1 {
		t.Fatalf("%d tickets after retry", n)
	}
}

func strp(s string) *string { return &s }

func (f *ticketFixture) principal(t *testing.T, s session) access.Principal {
	t.Helper()
	p, err := f.c.Auth.Authenticate(context.Background(), s.Access)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewerReplyAndDrafts(t *testing.T) {
	f := newTicketFixture(t)
	tk := f.createTicket(f.integration, map[string]any{}).JSON()
	key, seen := refOf(tk), tk["lastPublicMessageId"].(string)

	// Draft saved against what Dana has seen.
	d := f.do("PUT", tpath(key, "/drafts/public"), map[string]any{"body": "Thanks Priya, checking now.", "baseLastPublicId": seen},
		bearer(f.dana.Access)).expect(200).JSON()
	if d["version"] != float64(1) || d["baseLastPublicId"] != seen {
		t.Fatalf("draft: %v", d)
	}
	f.do("PUT", tpath(key, "/drafts/public"), map[string]any{"body": "stale tab"}, bearer(f.dana.Access), ifMatchV(5)).expect(409, "versionConflict")
	if got := f.do("GET", tpath(key, "/drafts/public"), nil, bearer(f.dana.Access)).expect(200).JSON(); got["body"] != "Thanks Priya, checking now." {
		t.Fatalf("draft read: %v", got)
	}
	// Drafts are private to their author.
	if got := f.do("GET", tpath(key, "/drafts/public"), nil, bearer(f.marco.Access)).expect(200).JSON(); got["body"] != "" || got["version"] != float64(0) {
		t.Fatalf("draft leaked: %v", got)
	}
	f.do("GET", tpath(key, "/drafts/customer"), nil, bearer(f.dana.Access)).expect(422)

	// A newer requester reply arrives.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Our contractor moved DNS yesterday."}, bearer(f.integration)).expect(201)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Thanks Priya, checking now.", "expectedLastPublicId": seen},
		bearer(f.dana.Access)).expect(409, "newerReplyExists")
	// The draft is kept for review.
	if got := f.do("GET", tpath(key, "/drafts/public"), nil, bearer(f.dana.Access)).expect(200).JSON(); got["body"] == "" {
		t.Fatal("draft lost on conflict")
	}
	latest := f.get(f.dana.Access, key).expect(200).Str("lastPublicMessageId")
	// An agent note does not count as a newer public reply.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "internal"}, bearer(f.marco.Access)).expect(201)
	m := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Thanks Priya, checking now.", "expectedLastPublicId": latest},
		bearer(f.dana.Access)).expect(201).JSON()
	if obj(m, "ticket")["lastPublicMessageId"] != m["id"] {
		t.Fatalf("last public id not advanced: %v", m)
	}
	// Sending removes the author's draft for that audience.
	if got := f.do("GET", tpath(key, "/drafts/public"), nil, bearer(f.dana.Access)).expect(200).JSON(); got["version"] != float64(0) {
		t.Fatalf("draft survived send: %v", got)
	}
	f.do("PUT", tpath(key, "/drafts/agent"), map[string]any{"body": "x"}, bearer(f.dana.Access)).expect(200)
	f.do("DELETE", tpath(key, "/drafts/agent"), nil, bearer(f.dana.Access)).expect(204)
}

func TestQueueViewsAndWatchers(t *testing.T) {
	f := newTicketFixture(t)
	a := f.createTicket(f.dana.Access, map[string]any{"subject": "A"}).Ref()
	b := f.createTicket(f.dana.Access, map[string]any{"subject": "B"}).Ref()
	c := f.createTicket(f.dana.Access, map[string]any{"subject": "C", "groupId": f.delivery}).Ref()
	f.do("POST", tpath(a, "/claim"), nil, bearer(f.dana.Access), ifMatchV(1)).expect(200)
	f.do("PUT", tpath(b, "/watchers/me"), nil, bearer(f.dana.Access)).expect(200)
	w := f.do("PUT", tpath(b, "/watchers/me"), nil, bearer(f.dana.Access)).expect(200).JSON() // idempotent
	if w["watching"] != true || w["watcherCount"] != float64(1) || w["version"] != float64(1) {
		t.Fatalf("watch: %v", w)
	}
	// Watching grants no access: Dana cannot see Delivery's ticket even
	// if a watcher row existed.
	f.do("PUT", tpath(c, "/watchers/me"), nil, bearer(f.dana.Access)).expect(404)

	view := func(s session, v string) []string {
		return keys(f.do("GET", "/api/v1/tickets?view="+v, nil, bearer(s.Access)).expect(200).Items())
	}
	if got := view(f.dana, "myWork"); len(got) != 1 || got[0] != a {
		t.Fatalf("myWork %v", got)
	}
	if got := view(f.dana, "unassigned"); len(got) != 1 || got[0] != b {
		t.Fatalf("unassigned %v", got)
	}
	if got := view(f.dana, "watching"); len(got) != 1 || got[0] != b {
		t.Fatalf("watching %v", got)
	}
	if got := view(f.dana, "dueSoon"); len(got) != 0 {
		t.Fatalf("dueSoon %v", got)
	}
	if got := view(f.marco, "unassigned"); len(got) != 2 {
		t.Fatalf("marco unassigned %v", got)
	}
	if got := keys(f.do("GET", "/api/v1/tickets?groupId="+f.delivery, nil, bearer(f.dana.Access)).expect(200).Items()); len(got) != 0 {
		t.Fatalf("group filter leaked %v", got)
	}
	f.do("GET", "/api/v1/tickets?view=nope", nil, bearer(f.dana.Access)).expect(422)
	f.do("GET", "/api/v1/tickets?view=myWork", nil, bearer(f.integration)).expect(403, "userRequired")
	counts := f.do("GET", "/api/v1/queue-counts", nil, bearer(f.dana.Access)).expect(200).JSON()
	if counts["myWork"] != float64(1) || counts["unassigned"] != float64(1) || counts["watching"] != float64(1) ||
		counts["awaitingCustomer"] != float64(0) || counts["onHold"] != float64(0) || counts["dueSoon"] != float64(0) {
		t.Fatalf("counts: %v", counts)
	}
	// Watcher-only access ends when membership ends.
	f.do("DELETE", "/api/v1/groups/"+f.triage+"/members/"+f.dana.UserId, nil, bearer(f.admin.Access)).expect(204)
	f.get(f.dana.Access, b).expect(404)
	if got := view(f.dana, "watching"); len(got) != 0 {
		t.Fatalf("watching after removal %v", got)
	}
}

func TestTicketsRequireAuthentication(t *testing.T) {
	f := newTicketFixture(t)
	if r := f.do("GET", "/api/v1/tickets", nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", r.Code)
	}
}

// Review findings (2026-10-02): staff activity must not leak through
// updatedAt/version; crash retries with attachments; message idempotency.
func TestIntegrationViewHidesStaffActivity(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	other := f.createTicket(f.integration, map[string]any{"subject": "Second"}).Ref()
	before := f.get(f.integration, key).expect(200).JSON()
	if before["version"] != float64(0) {
		t.Fatalf("integration sees version %v", before["version"])
	}
	time.Sleep(5 * time.Millisecond)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "internal"}, bearer(f.dana.Access)).expect(201)
	v := f.version(key)
	f.do("POST", tpath(key, "/assignments"), map[string]any{"groupId": f.triage, "assigneeId": f.marco.UserId}, bearer(f.lead.Access), ifMatchV(v)).expect(200)
	after := f.get(f.integration, key).expect(200).JSON()
	// The assignment keeps the ticket Open; the requester side
	// may see; reset by checking a staff-only change on its own.
	if obj(after, "status")["name"] != "Open" {
		t.Fatalf("status: %v", after["status"])
	}
	mid := after["updatedAt"]
	time.Sleep(5 * time.Millisecond)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "another note"}, bearer(f.marco.Access)).expect(201)
	f.do("PUT", tpath(key, "/watchers/me"), nil, bearer(f.marco.Access)).expect(200)
	final := f.get(f.integration, key).expect(200).JSON()
	if final["updatedAt"] != mid || final["activityAt"] != before["activityAt"] || final["version"] != float64(0) {
		t.Fatalf("staff-only changes visible to integration: %v → %v", mid, final["updatedAt"])
	}
	// List order for integrations follows requester-visible updates only:
	// the note on `key` must not move it above `other` if `other` changed later publicly.
	time.Sleep(5 * time.Millisecond)
	f.do("POST", tpath(other, "/messages"), map[string]any{"visibility": "public", "body": "bump"}, bearer(f.integration)).expect(201)
	time.Sleep(5 * time.Millisecond)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "late note"}, bearer(f.dana.Access)).expect(201)
	if ks := keys(f.do("GET", "/api/v1/tickets", nil, bearer(f.integration)).expect(200).Items()); ks[0] != other {
		t.Fatalf("integration order reveals staff activity: %v", ks)
	}
	if ks := keys(f.do("GET", "/api/v1/tickets", nil, bearer(f.dana.Access)).expect(200).Items()); ks[0] != key {
		t.Fatalf("staff order: %v", ks)
	}
}

func TestIdempotentRetriesAfterCommit(t *testing.T) {
	f := newTicketFixture(t)
	ctx := context.Background()
	scope, err := f.c.Access.Scope(ctx, f.principal(t, f.dana))
	if err != nil {
		t.Fatal(err)
	}
	att := f.upload(f.dana.Access, "a.txt", []byte("hello")).expect(201).Str("id")
	ref := idempotency.Ref{Scope: "user:" + f.dana.UserId + ":retry", Key: "k-att"}
	if _, err := f.c.Idempotency.Begin(ctx, ref.Scope, ref.Key, strings.Repeat("b", 64), time.Now()); err != nil {
		t.Fatal(err)
	}
	in := tickets.CreateInput{Subject: "With file", Body: "b", AttachmentIds: []string{att},
		Requester: tickets.RequesterInput{Name: strp("A"), Email: strp("a@b.example")}}
	t1, err := f.c.Tickets.Create(idempotency.WithRef(ctx, ref), scope, in)
	if err != nil {
		t.Fatal(err)
	}
	// The retry finds the committed ticket although the file is no longer staged.
	t2, err := f.c.Tickets.Create(idempotency.WithRef(ctx, ref), scope, in)
	if err != nil || t2.Id != t1.Id {
		t.Fatalf("retry: %v %v", tnum(t2), err)
	}
	// Abandon never drops a reservation that records a committed resource.
	if err := f.c.Idempotency.Abandon(ctx, ref.Scope, ref.Key); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.coll("idempotencyRecords").CountDocuments(ctx, bson.D{{Key: "key", Value: "k-att"}}); n != 1 {
		t.Fatal("committed reservation was abandoned")
	}

	// Messages: same protection against duplicate public replies.
	mref := idempotency.Ref{Scope: "user:" + f.dana.UserId + ":msg", Key: "k-msg"}
	if _, err := f.c.Idempotency.Begin(ctx, mref.Scope, mref.Key, strings.Repeat("c", 64), time.Now()); err != nil {
		t.Fatal(err)
	}
	post := tickets.PostInput{Visibility: "public", Body: "Only once"}
	m1, err := f.c.Tickets.PostMessage(idempotency.WithRef(ctx, mref), scope, tnum(t1), post)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := f.c.Tickets.PostMessage(idempotency.WithRef(ctx, mref), scope, tnum(t1), post)
	if err != nil || m2.Message.Id != m1.Message.Id {
		t.Fatalf("message retry: %v %v", m2.Message.Id, err)
	}
	if n, _ := f.coll("messages").CountDocuments(ctx, bson.D{{Key: "body", Value: "Only once"}}); n != 1 {
		t.Fatalf("%d copies of the reply", n)
	}
	// HTTP replay re-checks ticket access: Dana loses the group.
	body := map[string]any{"visibility": "agent", "body": "secret note"}
	f.do("POST", tpath(tnum(t1), "/messages"), body, bearer(f.dana.Access), header("Idempotency-Key", "n1")).expect(201)
	f.do("DELETE", "/api/v1/groups/"+f.triage+"/members/"+f.dana.UserId, nil, bearer(f.admin.Access)).expect(204)
	f.do("POST", tpath(tnum(t1), "/messages"), body, bearer(f.dana.Access), header("Idempotency-Key", "n1")).expect(404, "ticketNotFound")
}

func TestMessagePagingFollowsCommitOrder(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	for i := range 5 {
		f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": fmt.Sprintf("m%d", i)}, bearer(f.dana.Access)).expect(201)
	}
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
	if strings.Join(bodies[1:], ",") != "m0,m1,m2,m3,m4" {
		t.Fatalf("paged bodies: %v", bodies)
	}
}

func TestSubjectLockedExceptCreatingIntegration(t *testing.T) {
	f := newTicketFixture(t)
	other := f.do("POST", "/api/v1/integrations", map[string]any{"name": "CRM",
		"scopes": []string{"tickets:read", "tickets:create"}}, bearer(f.admin.Access)).expect(201).Str("token")
	web := f.createTicket(f.dana.Access, map[string]any{"subject": "Web subject"}).Ref()
	api := f.createTicket(f.integration, map[string]any{"subject": "API subject"}).Ref()

	// Users (agent, lead, installation admin) cannot change any subject.
	for _, s := range []session{f.dana, f.lead, f.admin} {
		for _, key := range []string{web, api} {
			f.do("PATCH", tpath(key), map[string]any{"subject": "Changed"}, bearer(s.Access), ifMatchV(f.version(key))).expect(403, "subjectLocked")
			f.do("PATCH", tpath(key), map[string]any{"subject": "Changed", "priority": 2}, bearer(s.Access), ifMatchV(f.version(key))).expect(403, "subjectLocked")
		}
	}
	// Another integration, and the creating integration on a web ticket: locked.
	f.do("PATCH", tpath(api), map[string]any{"subject": "Changed"}, bearer(other)).expect(403, "subjectLocked")
	f.do("PATCH", tpath(web), map[string]any{"subject": "Changed"}, bearer(f.integration)).expect(403, "subjectLocked")
	// Integrations may change the subject only.
	f.do("PATCH", tpath(api), map[string]any{"subject": "Changed", "priority": 1}, bearer(f.integration)).expect(403, "userRequired")
	f.do("PATCH", tpath(api), map[string]any{"priority": 1}, bearer(f.integration)).expect(403, "userRequired")
	f.do("PATCH", tpath(api), map[string]any{"subject": ""}, bearer(f.integration)).expect(422)
	// A read-only integration lacks tickets:write.
	f.do("PATCH", tpath(api), map[string]any{"subject": "Changed"}, bearer(f.readOnlyIntegraton)).expect(403)
	if got := f.get(f.dana.Access, api).expect(200).JSON(); got["subject"] != "API subject" || got["version"] != float64(1) {
		t.Fatalf("refused patches changed the ticket: %v", got)
	}

	// The creating integration may change it, with or without If-Match 0.
	r := f.do("PATCH", tpath(api), map[string]any{"subject": "Renamed by gateway"}, bearer(f.integration)).expect(200).JSON()
	if r["subject"] != "Renamed by gateway" || r["version"] != float64(0) {
		t.Fatalf("integration patch: %v", r)
	}
	f.do("PATCH", tpath(api), map[string]any{"subject": "Renamed again"}, bearer(f.integration), ifMatchV(0)).expect(200)
	got := f.get(f.dana.Access, api).expect(200).JSON()
	if got["subject"] != "Renamed again" || got["version"] != float64(3) {
		t.Fatalf("after integration patches: %v", got)
	}
	if !contains(f.auditActions(), "ticket.updated:success") {
		t.Fatal("subject change not audited")
	}
}

func TestWatchersListIsAccessFiltered(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{"subject": "Watched"}).Ref()
	for _, s := range []session{f.dana, f.marco, f.lead} {
		f.do("PUT", tpath(key, "/watchers/me"), nil, bearer(s.Access)).expect(200)
	}
	names := func(s session) []string {
		var out []string
		for _, w := range f.do("GET", tpath(key, "/watchers"), nil, bearer(s.Access)).expect(200).Items() {
			if w["id"] == "" || w["name"] == "" {
				t.Fatalf("watcher: %v", w)
			}
			out = append(out, w["name"].(string))
		}
		return out
	}
	if got := names(f.dana); strings.Join(got, ",") != "Dana Okafor,Marco Silva,Lea Lead" {
		t.Fatalf("watchers: %v", got)
	}
	// Integrations never list watchers.
	f.do("GET", tpath(key, "/watchers"), nil, bearer(f.integration)).expect(403, "userRequired")
	// A watcher who lost access is left out (the row and count remain).
	f.do("DELETE", "/api/v1/groups/"+f.triage+"/members/"+f.marco.UserId, nil, bearer(f.admin.Access)).expect(204)
	if got := names(f.dana); strings.Join(got, ",") != "Dana Okafor,Lea Lead" {
		t.Fatalf("watchers after removal: %v", got)
	}
	if got := f.get(f.dana.Access, key).expect(200).JSON(); got["watcherCount"] != float64(3) {
		t.Fatalf("count: %v", got)
	}
	// Callers who cannot read the ticket get 404, as for the ticket itself.
	f.do("GET", tpath(key, "/watchers"), nil, bearer(f.marco.Access)).expect(404)
	// A disabled account is left out too.
	uv := f.do("GET", "/api/v1/users/"+f.lead.UserId, nil, bearer(f.admin.Access)).expect(200).Version()
	f.do("PATCH", "/api/v1/users/"+f.lead.UserId, map[string]any{"status": "disabled"}, bearer(f.admin.Access), ifMatchV(uv)).expect(200)
	if got := names(f.dana); strings.Join(got, ",") != "Dana Okafor" {
		t.Fatalf("watchers after disable: %v", got)
	}
}
