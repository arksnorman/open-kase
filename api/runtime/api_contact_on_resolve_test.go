package runtime

import (
	"context"
	"net/url"
	"slices"
	"testing"
)

// auditChanges returns the changes of the audit events of one action on a ticket.
func (f *ticketFixture) auditChanges(ticketId, action string) [][]string {
	f.t.Helper()
	all, err := f.c.Audit.RecentAudit(context.Background(), 5000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out [][]string
	for _, ev := range all {
		if ev.TicketId == ticketId && ev.Action == action {
			out = append(out, ev.Changes)
		}
	}
	return out
}

func (f *ticketFixture) ticketId(key string) string {
	f.t.Helper()
	return f.get(f.admin.Access, key).expect(200).JSON()["id"].(string)
}

// orgContacts creates Acme (Priya, Sam), Globex (Gus) and a contact without an organisation.
func (f *ticketFixture) orgContacts() (acme, priya, sam, gus, loner string) {
	f.t.Helper()
	acme = f.createOrg(f.admin.Access, "Acme Ltd", "")["id"].(string)
	globex := f.createOrg(f.admin.Access, "Globex", "")["id"].(string)
	priya = f.contact("Priya Raman", "priya@acme.example", acme, "")
	sam = f.contact("Sam Teammate", "sam@acme.example", acme, "")
	gus = f.contact("Gus Other", "gus@globex.example", globex, "")
	loner = f.contact("Lone Wolf", "lone@x.example", "", "")
	return
}

func TestRequesterResolveAsOrganisationContact(t *testing.T) {
	f := newTicketFixture(t)
	acme, priya, sam, gus, loner := f.orgContacts()
	_, tok := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	_, staffTok := f.token("Back office", "tickets:read", "tickets:write")
	mk := func() string {
		return f.createTicket(f.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya}}).Ref()
	}
	resolve := func(key, bearerTok string, body map[string]any) result {
		t.Helper()
		b := map[string]any{"action": "resolve"}
		for k, v := range body {
			b[k] = v
		}
		return f.do("POST", tpath(key, "/transitions"), b, bearer(bearerTok))
	}

	// Rejections leave the ticket Open, for any contact that is not a teammate.
	key := mk()
	resolve(key, tok, map[string]any{"contactId": gus}).expectFieldCode(t, "contactNotInOrganisation")
	resolve(key, tok, map[string]any{"contactId": loner}).expectFieldCode(t, "contactNotInOrganisation")
	resolve(key, tok, map[string]any{"contactId": "000000000000000000000000"}).expectFieldCode(t, "contactNotInOrganisation")
	resolve(key, tok, map[string]any{"contactId": "bad"}).expect(422)
	if f.statusCategory(key) != "open" {
		t.Fatalf("category after rejected resolves: %s", f.statusCategory(key))
	}
	// Not allowed for users, staff tokens and other actions.
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve", "contactId": sam}, bearer(f.dana.Access), ifMatchV(f.version(key))).expectFieldCode(t, "notAllowed")
	resolve(key, staffTok, map[string]any{"contactId": sam}).expect(428)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve", "contactId": sam}, bearer(staffTok), ifMatchV(f.version(key))).expectFieldCode(t, "notAllowed")
	// A token with a staff scope is staff, even when it also holds replyAsRequester.
	_, bothTok := f.token("Both", "tickets:read", "tickets:write", "tickets:replyAsRequester")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve", "contactId": sam}, bearer(bothTok), ifMatchV(f.version(key))).expectFieldCode(t, "notAllowed")
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive", "contactId": sam}, bearer(tok)).expect(403, "userRequired")
	if f.statusCategory(key) != "open" {
		t.Fatalf("category after refused resolves: %s", f.statusCategory(key))
	}

	// A ticket without an organisation never accepts another contact.
	plain := f.createTicket(f.integration, map[string]any{"requester": map[string]any{"contactId": priya}}).Ref()
	resolve(plain, tok, map[string]any{"contactId": sam}).expectFieldCode(t, "contactNotInOrganisation")

	// The teammate resolves: the audit event names the contact.
	resolve(key, tok, map[string]any{"contactId": sam}).expect(200)
	id := f.ticketId(key)
	got := f.auditChanges(id, "ticket.resolved")
	if len(got) != 1 || !slices.Contains(got[0], "contact:"+sam) {
		t.Fatalf("resolved audit changes: %v", got)
	}
	if tk := f.get(f.dana.Access, key).expect(200).JSON(); obj(tk, "requester")["id"] != priya {
		t.Fatalf("requester changed: %v", tk["requester"])
	}

	// Validated before the repeat shortcut; a valid repeat writes nothing.
	v := f.version(key)
	resolve(key, tok, map[string]any{"contactId": gus}).expectFieldCode(t, "contactNotInOrganisation")
	resolve(key, tok, map[string]any{"contactId": sam}).expect(200)
	if f.version(key) != v || len(f.auditChanges(id, "ticket.resolved")) != 1 {
		t.Fatalf("a repeated resolve wrote something: version %d -> %d", v, f.version(key))
	}

	// The requester's own id is accepted and leaves no mark.
	own := mk()
	resolve(own, tok, map[string]any{"contactId": priya}).expect(200)
	if got := f.auditChanges(f.ticketId(own), "ticket.resolved"); len(got) != 1 || slices.ContainsFunc(got[0], func(c string) bool { return len(c) > 8 && c[:8] == "contact:" }) {
		t.Fatalf("own id left a mark: %v", got)
	}
}

func TestMCPRequesterResolveAsOrganisationContact(t *testing.T) {
	f := newMCPFixture(t)
	acme, priya, sam, gus, _ := f.orgContacts()
	key := f.createTicket(f.ticketFixture.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya}}).Ref()
	_, tok := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	_, staffTok := f.token("Back office", "tickets:read", "tickets:write")
	s := f.connect(tok)

	mustFail(t, s, "ticketsTransition", map[string]any{"ticket": key, "action": "resolve", "contactId": gus}, "validationFailed")
	mustFail(t, s, "ticketsTransition", map[string]any{"ticket": key, "action": "resolve", "contactId": "000000000000000000000000"}, "validationFailed")
	mustFail(t, f.connect(staffTok), "ticketsTransition", map[string]any{"ticket": key, "action": "resolve", "contactId": sam}, "validationFailed")
	mustFail(t, s, "ticketsTransition", map[string]any{"ticket": key, "action": "archive", "contactId": sam}, "userRequired")
	if f.statusCategory(key) != "open" {
		t.Fatalf("category after rejected resolves: %s", f.statusCategory(key))
	}
	out := mustCall(t, s, "ticketsTransition", map[string]any{"ticket": key, "action": "resolve", "contactId": sam})
	if obj(out, "status")["category"] != "resolved" {
		t.Fatalf("resolve: %v", out)
	}
	got := f.auditChanges(f.ticketId(key), "ticket.resolved")
	if len(got) != 1 || !slices.Contains(got[0], "contact:"+sam) {
		t.Fatalf("resolved audit changes: %v", got)
	}
	// Repeating it answers with the ticket and writes nothing.
	v := f.version(key)
	out = mustCall(t, s, "ticketsTransition", map[string]any{"ticket": key, "action": "resolve", "contactId": sam})
	if obj(out, "status")["category"] != "resolved" || f.version(key) != v || len(f.auditChanges(f.ticketId(key), "ticket.resolved")) != 1 {
		t.Fatalf("repeat: %v", out)
	}
}

func TestFollowUpAsOrganisationContact(t *testing.T) {
	f := newTicketFixture(t)
	acme, priya, sam, gus, loner := f.orgContacts()
	_, tok := f.token("Portal", "tickets:read", "tickets:create")
	_, noBot := f.token("Back office", "tickets:read", "tickets:create", "tickets:write")
	_, bot := f.token("Bot", "tickets:read", "tickets:create", "tickets:comment")
	_, all := f.token("Everything", "tickets:all")
	orig := f.createTicket(f.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya}}).Ref()
	followUp := func(tok string, body map[string]any) result {
		t.Helper()
		return f.do("POST", tpath(orig, "/follow-up"), body, bearer(tok))
	}

	// Rejections create nothing.
	followUp(tok, map[string]any{"contactId": gus}).expectFieldCode(t, "contactNotInOrganisation")
	followUp(tok, map[string]any{"contactId": loner}).expectFieldCode(t, "contactNotInOrganisation")
	followUp(tok, map[string]any{"contactId": "000000000000000000000000"}).expectFieldCode(t, "contactNotInOrganisation")
	followUp(tok, map[string]any{"contactId": "bad"}).expect(422)
	followUp(bot, map[string]any{"contactId": sam}).expectFieldCode(t, "notAllowed")
	followUp(all, map[string]any{"contactId": sam}).expectFieldCode(t, "notAllowed")
	followUp(f.dana.Access, map[string]any{"contactId": sam}).expectFieldCode(t, "notAllowed")
	if n := len(f.do("GET", "/api/v1/tickets", nil, bearer(f.admin.Access)).expect(200).Items()); n != 1 {
		t.Fatalf("tickets after rejections: %d", n)
	}

	// A token that writes as the requester (with or without staff scopes).
	for _, token := range []string{tok, noBot} {
		key := followUp(token, map[string]any{"contactId": sam, "body": "From Sam"}).expect(201).Ref()
		tk := f.get(f.admin.Access, key).expect(200).JSON()
		if obj(tk, "requester")["id"] != priya {
			t.Fatalf("follow-up requester: %v", tk["requester"])
		}
		msgs := f.do("GET", tpath(key, "/messages"), nil, bearer(f.admin.Access)).expect(200).Items()
		if len(msgs) != 1 || obj(msgs[0], "author")["id"] != sam || obj(msgs[0], "author")["context"] != "contact" {
			t.Fatalf("first message: %v", msgs)
		}
		got := f.auditChanges(tk["id"].(string), "ticket.created")
		if len(got) != 1 || !slices.Contains(got[0], "contact:"+sam) {
			t.Fatalf("created audit changes: %v", got)
		}
	}

	// The requester's own id, or none, authors as the requester without a mark.
	for _, body := range []map[string]any{{"contactId": priya}, {}} {
		key := followUp(tok, body).expect(201).Ref()
		tk := f.get(f.admin.Access, key).expect(200).JSON()
		msgs := f.do("GET", tpath(key, "/messages"), nil, bearer(f.admin.Access)).expect(200).Items()
		if obj(msgs[0], "author")["id"] != priya {
			t.Fatalf("author: %v", msgs[0])
		}
		if got := f.auditChanges(tk["id"].(string), "ticket.created"); slices.ContainsFunc(got[0], func(c string) bool { return len(c) > 8 && c[:8] == "contact:" }) {
			t.Fatalf("mark on %v", got)
		}
	}

	// A ticket without an organisation never accepts another contact.
	plain := f.createTicket(f.integration, map[string]any{"requester": map[string]any{"contactId": priya}}).Ref()
	f.do("POST", tpath(plain, "/follow-up"), map[string]any{"contactId": sam}, bearer(tok)).expectFieldCode(t, "contactNotInOrganisation")

	// An archived original still takes a teammate's follow-up.
	for _, action := range []string{"resolve", "archive"} {
		f.do("POST", tpath(orig, "/transitions"), map[string]any{"action": action}, bearer(f.admin.Access), ifMatchV(f.version(orig))).expect(200)
	}
	late := followUp(tok, map[string]any{"contactId": sam, "body": "Back again"}).expect(201).Ref()
	if msgs := f.do("GET", tpath(late, "/messages"), nil, bearer(f.admin.Access)).expect(200).Items(); len(msgs) != 1 || obj(msgs[0], "author")["id"] != sam {
		t.Fatalf("follow-up to an archived ticket: %v", msgs)
	}
}

// The acting contact reaches webhook receivers as contactId on
// ticket.statusChanged (a resolve, a reply that reopens) and ticket.created
// (a follow-up), and only when it is not the requester.
func TestActingContactInWebhooks(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	f.addHook(hs, "ticket.statusChanged", "ticket.created")
	acme, priya, sam, _, _ := f.orgContacts()
	_, tok := f.token("Portal", "tickets:read", "tickets:create", "tickets:replyAsRequester")
	key := f.createTicket(f.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya}}).Ref()

	// last drains the outbox and returns the newest payload of a type and how many arrived.
	last := func(typ string) (map[string]any, int) {
		t.Helper()
		f.drain()
		got := hookEnvelopes(t, hs)[typ]
		if len(got) == 0 {
			t.Fatalf("no %s webhook", typ)
		}
		return obj(got[len(got)-1], "data"), len(got)
	}
	want := func(d map[string]any, transition, by string, contact any) {
		t.Helper()
		if d["transition"] != transition || d["by"] != by {
			t.Fatalf("want %s by %s: %v", transition, by, d)
		}
		if got, has := d["contactId"]; (contact == nil) == has || (has && got != contact) {
			t.Fatalf("contactId %v (present %v), want %v: %v", got, has, contact, d)
		}
		if obj(d, "requester")["id"] != priya {
			t.Fatalf("requester: %v", d["requester"])
		}
	}
	resolve := func(body map[string]any) {
		t.Helper()
		body["action"] = "resolve"
		f.do("POST", tpath(key, "/transitions"), body, bearer(tok)).expect(200)
	}
	reply := func(body map[string]any) {
		t.Helper()
		body["visibility"], body["body"], body["as"] = "public", "Still broken", "requester"
		f.do("POST", tpath(key, "/messages"), body, bearer(tok)).expect(201)
	}

	if d, _ := last("ticket.created"); d["contactId"] != nil {
		t.Fatalf("created: %v", d)
	}
	// A teammate resolves; repeating it sends nothing more.
	resolve(map[string]any{"contactId": sam})
	d, n := last("ticket.statusChanged")
	want(d, "resolve", "requester", sam)
	resolve(map[string]any{"contactId": sam})
	if _, again := last("ticket.statusChanged"); again != n {
		t.Fatalf("a repeated resolve sent a webhook: %d -> %d", n, again)
	}
	// The teammate's reply reopens it.
	reply(map[string]any{"contactId": sam})
	d, _ = last("ticket.statusChanged")
	want(d, "requesterReply", "requester", sam)
	// The requester, with or without their own id, and staff: no contactId.
	resolve(map[string]any{})
	d, _ = last("ticket.statusChanged")
	want(d, "resolve", "requester", nil)
	reply(map[string]any{"contactId": priya})
	d, _ = last("ticket.statusChanged")
	want(d, "requesterReply", "requester", nil)
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	d, _ = last("ticket.statusChanged")
	want(d, "resolve", "staff", nil)

	// Follow-ups: the teammate's carries contactId, the requester's does not.
	f.do("POST", tpath(key, "/follow-up"), map[string]any{"contactId": sam}, bearer(tok)).expect(201)
	if d, _ := last("ticket.created"); d["contactId"] != sam || obj(d, "requester")["id"] != priya {
		t.Fatalf("teammate follow-up: %v", d)
	}
	f.do("POST", tpath(key, "/follow-up"), map[string]any{}, bearer(tok)).expect(201)
	if d, _ := last("ticket.created"); d["contactId"] != nil {
		t.Fatalf("requester follow-up: %v", d)
	}
}

func TestMCPFollowUpAsOrganisationContact(t *testing.T) {
	f := newMCPFixture(t)
	acme, priya, sam, gus, _ := f.orgContacts()
	orig := f.createTicket(f.ticketFixture.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya}}).Ref()
	_, tok := f.token("Portal", "tickets:read", "tickets:create")
	_, bot := f.token("Bot", "tickets:read", "tickets:create", "tickets:comment")
	mustFail(t, f.connect(tok), "ticketsFollowUp", map[string]any{"ticket": orig, "contactId": gus}, "validationFailed")
	mustFail(t, f.connect(bot), "ticketsFollowUp", map[string]any{"ticket": orig, "contactId": sam}, "validationFailed")
	out := mustCall(t, f.connect(tok), "ticketsFollowUp", map[string]any{"ticket": orig, "contactId": sam, "body": "From Sam"})
	key := refOf(out)
	tk := f.get(f.admin.Access, key).expect(200).JSON()
	if obj(tk, "requester")["id"] != priya {
		t.Fatalf("follow-up requester: %v", tk["requester"])
	}
	msgs := f.do("GET", tpath(key, "/messages"), nil, bearer(f.admin.Access)).expect(200).Items()
	if len(msgs) != 1 || obj(msgs[0], "author")["id"] != sam {
		t.Fatalf("first message: %v", msgs)
	}
	if got := f.auditChanges(tk["id"].(string), "ticket.created"); len(got) != 1 || !slices.Contains(got[0], "contact:"+sam) {
		t.Fatalf("created audit changes: %v", got)
	}
}

func TestMCPListStaffFilters(t *testing.T) {
	f := newMCPFixture(t)
	f.do("POST", "/api/v1/tags", map[string]any{"name": "vip"}, bearer(f.admin.Access)).expect(201)
	a := f.createTicket(f.dana.Access, map[string]any{"subject": "Critical one", "priority": 1}).Ref()
	b := f.createTicket(f.dana.Access, map[string]any{"subject": "Normal one"}).Ref()
	f.do("PATCH", tpath(b), map[string]any{"tags": []string{"vip"}}, bearer(f.dana.Access), ifMatchV(f.version(b))).expect(200)
	requestType := obj(f.get(f.admin.Access, a).expect(200).JSON(), "requestType")["id"].(string)
	_, staff := f.token("Back office", "tickets:read", "tickets:write")
	s := f.connect(staff)
	// list returns the MCP result, which must be what REST answers for the same filters.
	list := func(args map[string]any) []string {
		t.Helper()
		out := []string{}
		items, _ := mustCall(t, s, "ticketsList", args)["items"].([]any)
		for _, it := range items {
			out = append(out, refOf(it.(map[string]any)))
		}
		q, rest := url.Values{}, []string{}
		for k, v := range args {
			q.Set(k, v.(string))
		}
		for _, it := range f.do("GET", "/api/v1/tickets?"+q.Encode(), nil, bearer(staff)).expect(200).Items() {
			rest = append(rest, refOf(it))
		}
		if !slices.Equal(out, rest) {
			t.Fatalf("%v: MCP %v, REST %v", args, out, rest)
		}
		return out
	}
	if got := list(map[string]any{"priority": "1"}); len(got) != 1 || got[0] != a {
		t.Fatalf("priority: %v", got)
	}
	if got := list(map[string]any{"assignee": "unassigned"}); len(got) != 2 {
		t.Fatalf("assignee: %v", got)
	}
	if got := list(map[string]any{"assignee": "me"}); len(got) != 0 {
		t.Fatalf("assignee me: %v", got)
	}
	if got := list(map[string]any{"priority": "1,3"}); len(got) != 2 {
		t.Fatalf("priority list: %v", got)
	}
	if got := list(map[string]any{"tag": "vip"}); len(got) != 1 || got[0] != b {
		t.Fatalf("tag: %v", got)
	}
	if got := list(map[string]any{"tag": "nosuchtag"}); len(got) != 0 {
		t.Fatalf("unknown tag: %v", got)
	}
	if got := list(map[string]any{"requestTypeId": requestType}); len(got) != 2 {
		t.Fatalf("request type: %v", got)
	}
	if got := list(map[string]any{"requestTypeId": "000000000000000000000000"}); len(got) != 0 {
		t.Fatalf("other request type: %v", got)
	}
	if got := list(map[string]any{"requestTypeId": requestType, "priority": "1", "assignee": "unassigned"}); len(got) != 1 || got[0] != a {
		t.Fatalf("combined: %v", got)
	}
	// due: whatever the SLA state is, MCP and REST agree (checked in list).
	for _, due := range []string{"running", "breached"} {
		list(map[string]any{"due": due})
	}
	mustFail(t, s, "ticketsList", map[string]any{"priority": "9"}, "validationFailed")
	mustFail(t, s, "ticketsList", map[string]any{"due": "never"}, "validationFailed")
	mustFail(t, s, "ticketsList", map[string]any{"requestTypeId": "bad"}, "validationFailed")

	// Requester-side tokens are refused.
	_, portal := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	ps := f.connect(portal)
	for _, args := range []map[string]any{{"assignee": "me"}, {"priority": "1"}, {"tag": "x"}, {"requestTypeId": "000000000000000000000000"}, {"due": "running"}} {
		mustFail(t, ps, "ticketsList", args, "userRequired")
	}
}

func (f *ticketFixture) statusCategory(key string) string {
	f.t.Helper()
	return category(f.get(f.admin.Access, key).expect(200))
}
