package runtime

import (
	"strings"
	"testing"
	"time"
)

// firstAuthor returns the author of the oldest message of a ticket.
func (f *ticketFixture) firstAuthor(key string) map[string]any {
	f.t.Helper()
	items := f.do("GET", tpath(key, "/messages"), nil, bearer(f.admin.Access)).expect(200).Items()
	if len(items) == 0 {
		f.t.Fatal("no messages")
	}
	return obj(items[0], "author")
}

func (f *ticketFixture) contact(name, email, org, externalId string) string {
	f.t.Helper()
	body := map[string]any{"name": name, "email": email}
	if org != "" {
		body["organisationId"] = org
	}
	if externalId != "" {
		body["externalId"] = externalId
	}
	return f.do("POST", "/api/v1/contacts", body, bearer(f.admin.Access)).JSON()["id"].(string)
}

// The first message of a ticket (and follow-up) is authored by the bot only
// when the token may speak as staff (tickets:comment or tickets:all).
func TestTokenCreateAuthorFollowsComment(t *testing.T) {
	f := newTicketFixture(t)
	cases := []struct {
		name   string
		scopes []string
		want   string
	}{
		{"write", []string{"tickets:read", "tickets:create", "tickets:write"}, "contact"},
		{"internal", []string{"tickets:read", "tickets:create", "tickets:internal"}, "contact"},
		{"comment", []string{"tickets:read", "tickets:create", "tickets:comment"}, "bot"},
		{"all", []string{"tickets:all"}, "bot"},
		{"requester", []string{"tickets:read", "tickets:create"}, "contact"},
	}
	for _, c := range cases {
		_, tok := f.token("Create "+c.name, c.scopes...)
		key := f.createTicket(tok, map[string]any{"priority": 1}).Ref()
		a := f.firstAuthor(key)
		if a["context"] != c.want {
			t.Fatalf("%s: first message author %v, want %s", c.name, a, c.want)
		}
		if c.want == "contact" && a["name"] != "Priya Raman" {
			t.Fatalf("%s: contact author %v", c.name, a)
		}
		// Priority follows the staff check, not the author.
		tk := f.get(f.admin.Access, key).expect(200).JSON()
		staff := c.name != "requester"
		if staff && tk["priority"] != float64(1) {
			t.Fatalf("%s: priority %v", c.name, tk["priority"])
		}
		if !staff && tk["priority"] != float64(3) {
			t.Fatalf("%s: requester-side priority %v", c.name, tk["priority"])
		}
		fu := f.do("POST", tpath(key, "/follow-up"), map[string]any{"body": "Following up"}, bearer(tok)).expect(201).Ref()
		if a := f.firstAuthor(fu); a["context"] != c.want {
			t.Fatalf("%s: follow-up author %v, want %s", c.name, a, c.want)
		}
	}
}

func TestRequesterSideListFilters(t *testing.T) {
	f := newTicketFixture(t)
	acme := f.createOrg(f.admin.Access, "Acme Ltd", "")["id"].(string)
	globex := f.createOrg(f.admin.Access, "Globex", "")["id"].(string)
	priya := f.contact("Priya Raman", "priya@acme.example", acme, "")
	sam := f.contact("Sam Teammate", "sam@acme.example", acme, "")
	gus := f.contact("Gus Other", "gus@globex.example", globex, "")
	mk := func(org, contact string) string {
		return f.createTicket(f.integration, map[string]any{"organisationId": org, "requester": map[string]any{"contactId": contact}}).Ref()
	}
	t1, t2, t3 := mk(acme, priya), mk(acme, sam), mk(globex, gus)
	_, writer := f.token("Resolver", "tickets:read", "tickets:write")
	f.do("POST", tpath(t1, "/transitions"), map[string]any{"action": "resolve"}, bearer(writer), ifMatchV(f.version(t1))).expect(200)

	list := func(tok, query string) []string {
		t.Helper()
		var out []string
		for _, it := range f.do("GET", "/api/v1/tickets?"+query, nil, bearer(tok)).expect(200).Items() {
			out = append(out, refOf(it))
		}
		return out
	}
	same := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		seen := map[string]bool{}
		for _, g := range got {
			seen[g] = true
		}
		for _, w := range want {
			if !seen[w] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}
	for _, tok := range []string{f.integration, f.admin.Access} {
		same(list(tok, "organisationId="+acme), t1, t2)
		same(list(tok, "organisationId="+globex), t3)
		same(list(tok, "requesterId="+priya), t1)
		same(list(tok, "requesterId="+sam+"&organisationId="+acme), t2)
		same(list(tok, "requesterId="+sam+"&organisationId="+globex))
		same(list(tok, "statusId="+f.statusID("resolved")), t1)
		same(list(tok, "statusId="+f.statusID("resolved")+"&organisationId="+acme+"&requesterId="+priya), t1)
	}
	f.do("GET", "/api/v1/tickets?requesterId=bad", nil, bearer(f.integration)).expect(422)
	// Staff-only filters stay staff only for requester-side tokens.
	for _, q := range []string{"priority=1", "assignee=unassigned", "tag=vip", "requestTypeId=" + strings.Repeat("a", 24), "due=running"} {
		f.do("GET", "/api/v1/tickets?"+q, nil, bearer(f.integration)).expect(403, "userRequired")
		f.do("GET", "/api/v1/tickets?requesterId="+priya+"&"+q, nil, bearer(f.integration)).expect(403, "userRequired")
	}
	f.do("GET", "/api/v1/tickets?priority=1", nil, bearer(f.dana.Access)).expect(200)
}

func TestExactExternalLookups(t *testing.T) {
	f := newTicketFixture(t)
	o1 := f.createOrg(f.admin.Access, "One", "acct-1")["id"].(string)
	o10 := f.createOrg(f.admin.Access, "Ten", "acct-10")["id"].(string)
	c1 := f.contact("Ann One", "ann@one.example", "", "acct-1")
	c10 := f.contact("Ben Ten", "ben@ten.example", "", "acct-10")
	_, tok := f.token("Portal", "tickets:read", "contacts:read", "organisations:read")

	one := func(path, want string) {
		t.Helper()
		items := f.do("GET", path, nil, bearer(tok)).expect(200).Items()
		if len(items) != 1 || items[0]["id"] != want {
			t.Fatalf("%s: %v", path, items)
		}
	}
	one("/api/v1/contacts?externalId=acct-1", c1)
	one("/api/v1/contacts?externalId=acct-10", c10)
	one("/api/v1/organisations?externalRef=acct-1", o1)
	one("/api/v1/organisations?externalRef=acct-10", o10)
	if n := len(f.do("GET", "/api/v1/contacts?externalId=acct", nil, bearer(tok)).expect(200).Items()); n != 0 {
		t.Fatalf("prefix matched %d contacts", n)
	}
	if n := len(f.do("GET", "/api/v1/organisations?externalRef=acct", nil, bearer(tok)).expect(200).Items()); n != 0 {
		t.Fatalf("prefix matched %d organisations", n)
	}
	if n := len(f.do("GET", "/api/v1/organisations?externalRef=nope", nil, bearer(tok)).expect(200).Items()); n != 0 {
		t.Fatalf("unknown ref matched %d", n)
	}
}

func TestWebhookTeammateAuthorIdAndOrganisationValues(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	f.addHook(hs, "ticket.created", "ticket.message.public")
	acme := f.createOrg(f.admin.Access, "Acme Ltd", "CUST-0042")["id"].(string)
	priya := f.contact("Priya Raman", "priya@acme.example", acme, "crm-1042")
	sam := f.contact("Sam Teammate", "sam@acme.example", acme, "crm-2000")
	_, tok := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	key := f.createTicket(f.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya}}).Ref()
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "From Sam", "as": "requester", "contactId": sam}, bearer(tok)).expect(201)
	f.reply(f.dana.Access, key, "public", "From Dana")
	f.drain()
	by := hookEnvelopes(t, hs)

	for _, typ := range []string{"ticket.created", "ticket.message.public"} {
		d := obj(by[typ][0], "data")
		org := obj(d, "organisation")
		if org["id"] != acme || org["name"] != "Acme Ltd" || org["externalRef"] != "CUST-0042" {
			t.Fatalf("%s organisation: %v", typ, org)
		}
		if req := obj(d, "requester"); req["id"] != priya || req["externalId"] != "crm-1042" {
			t.Fatalf("%s requester: %v", typ, req)
		}
	}
	var authors []map[string]any
	for _, env := range by["ticket.message.public"] {
		if m := obj(obj(env, "data"), "message"); m != nil {
			authors = append(authors, obj(m, "author"))
		}
	}
	var sawSam, sawDana bool
	for _, a := range authors {
		switch a["context"] {
		case "contact":
			if a["id"] == sam && a["name"] == "Sam Teammate" {
				sawSam = true
			}
		case "staff":
			sawDana = true
			if _, ok := a["id"]; ok {
				t.Fatalf("staff author id exposed to a requester-side endpoint: %v", a)
			}
		}
	}
	if !sawSam || !sawDana {
		t.Fatalf("authors: %v", authors)
	}
}

func TestSettingsPutKeepsRequesterLink(t *testing.T) {
	f := newNotifyFixture(t)
	const link = "https://portal.example.com/requests"
	put, v := f.settingsPut(map[string]any{"notifications": map[string]any{"requesterLinkBaseUrl": link}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	put, v = f.settingsPut(map[string]any{"notifications": map[string]any{"requesterEmails": false}})
	got := obj(f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON(), "notifications")
	if got["requesterLinkBaseUrl"] != link || got["requesterEmails"] != false {
		t.Fatalf("link wiped or switch not set: %v", got)
	}
	put, v = f.settingsPut(map[string]any{"notifications": map[string]any{"requesterLinkBaseUrl": ""}})
	got = obj(f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON(), "notifications")
	if got["requesterLinkBaseUrl"] != "" {
		t.Fatalf("empty string must clear the link: %v", got)
	}
}

func TestRequesterEmailsOffSkipsResolveNoticeAndReminder(t *testing.T) {
	f := newAutomationFixture(t)
	f.do("PUT", "/api/v1/automation", map[string]any{"archiveAfterResolvedDays": 3, "customerFollowupAfterHours": 24, "maxReminders": 2,
		"reminderCooldownHours": 24, "autoResolveAfterHours": 48}, bearer(f.admin.Access), header("If-Match", "0")).expect(200)
	put, v := f.settingsPut(map[string]any{"notifications": map[string]any{"requesterEmails": false}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200)

	k := f.createTicket(f.integration, map[string]any{"ccEmails": []string{"ops@acme.example"}}).Ref()
	f.waiting(f.dana.Access, k)
	f.advance(25 * time.Hour)
	f.runDue()
	k2 := f.createTicket(f.integration, map[string]any{"subject": "Second"}).Ref()
	f.do("POST", tpath(k2, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(k2))).expect(200)
	f.runDue()
	if len(sentTo(f.mail, "priya@acme.example")) != 0 || len(sentTo(f.mail, "ops@acme.example")) != 0 {
		t.Fatalf("requester emails sent while off: %+v", f.mail.Sent())
	}
	for _, typ := range []string{"ticket.statusChanged", "ticket.followupReminder"} {
		n := 0
		for _, d := range f.deliveriesFor(typ) {
			if d.Channel == "email" && (d.Recipient.Kind == "contact" || d.Recipient.Kind == "address") {
				n++
				if d.Status != "skipped" || d.ErrorClass != "requesterEmailsOff" {
					t.Fatalf("%s delivery: %+v", typ, d)
				}
			}
		}
		if n == 0 {
			t.Fatalf("no skipped %s delivery recorded", typ)
		}
	}
}

// A requester-side token with tickets:replyAsRequester closes its tickets;
// nothing else of the lifecycle is open to it.
func TestRequesterSideResolve(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	f.addHook(hs, "ticket.statusChanged")
	tokID, tok := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	_, noReply := f.token("Reader", "tickets:read")
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	resolve := func(tok string) result {
		return f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(tok))
	}

	// Without the scope and without staff scopes: 403.
	resolve(noReply).expect(403)
	resolve(f.readOnlyIntegraton).expect(403)

	// Every other action is staff only for the portal token.
	for _, body := range []map[string]any{
		{"action": "archive"}, {"action": "reopen"},
		{"action": "hold", "holdReasonId": f.holdReasonID("Waiting on Engineering")},
		{"action": "setStatus", "statusId": f.statusID("open")},
	} {
		f.do("POST", tpath(key, "/transitions"), body, bearer(tok)).expect(403, "userRequired")
	}
	if got := obj(f.get(f.admin.Access, key).expect(200).JSON(), "status")["name"]; got != "Open" {
		t.Fatalf("status changed by a rejected action: %v", got)
	}

	// Resolve without If-Match; the response is the requester-side view.
	r := resolve(tok).expect(200).JSON()
	if obj(r, "status")["name"] != "Resolved" || r["version"] != float64(0) {
		t.Fatalf("resolve response: %v", r)
	}
	if got := obj(f.get(f.admin.Access, key).expect(200).JSON(), "status")["name"]; got != "Resolved" {
		t.Fatalf("status %v", got)
	}
	// An If-Match is ignored; resolving again is invalidTransition.
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(tok), ifMatchV(99)).expect(409, "invalidTransition")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive"}, bearer(tok)).expect(403, "userRequired")

	// Audited with the token as actor; statusChanged webhook with transition resolve.
	var audited bool
	for _, ev := range f.tokenAudit(tokID) {
		if ev.Action == "ticket.resolved" && ev.ActorType == "service" {
			audited = true
		}
	}
	if !audited {
		t.Fatal("no ticket.resolved audit event for the token")
	}
	f.drain()
	got := hookEnvelopes(t, hs)["ticket.statusChanged"]
	if len(got) == 0 {
		t.Fatal("no statusChanged webhook")
	}
	if d := obj(got[len(got)-1], "data"); d["transition"] != "resolve" || obj(d, "status")["category"] != "resolved" {
		t.Fatalf("webhook: %v", d)
	}

	// The requester reopens by replying; an archived ticket stays final.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Still broken", "as": "requester"}, bearer(tok)).expect(201)
	if got := obj(f.get(f.admin.Access, key).expect(200).JSON(), "status")["name"]; got != "Open" {
		t.Fatalf("reply did not reopen: %v", got)
	}
	resolve(tok).expect(200)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	resolve(tok).expect(409, "ticketArchived")
}
