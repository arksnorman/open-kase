package runtime

import (
	"strings"
	"testing"
)

// expectFieldCode asserts a 422 validation problem carrying a field error with this code.
func (r result) expectFieldCode(t *testing.T, code string) {
	t.Helper()
	r.expect(422, "validationFailed")
	if !strings.Contains(string(r.Body), `"code":"`+code+`"`) {
		t.Fatalf("want field code %s: %s", code, r.Body)
	}
}

// Owner feedback: a reply posted as the requester may be authored by another
// contact of the ticket's organisation (contactId).
func TestReplyAsOrganisationContact(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	acme := f.createOrg(f.admin.Access, "Acme Ltd", "")["id"].(string)
	globex := f.createOrg(f.admin.Access, "Globex", "")["id"].(string)
	contact := func(name, email, org string) string {
		body := map[string]any{"name": name, "email": email}
		if org != "" {
			body["organisationId"] = org
		}
		r := f.do("POST", "/api/v1/contacts", body, bearer(f.admin.Access))
		return r.JSON()["id"].(string)
	}
	priya := contact("Priya Raman", "priya@acme.example", acme)
	sam := contact("Sam Teammate", "sam@acme.example", acme)
	gus := contact("Gus Other", "gus@globex.example", globex)
	loner := contact("Lone Wolf", "lone@x.example", "")
	_, tok := f.token("Gateway", "tickets:read", "tickets:replyAsRequester")
	_, both := f.token("Both", "tickets:read", "tickets:replyAsRequester", "tickets:comment")

	key := f.createTicket(f.integration, map[string]any{"organisationId": acme, "priority": 1,
		"requester": map[string]any{"contactId": priya}}).Ref()
	post := func(tok string, body map[string]any) result {
		t.Helper()
		b := map[string]any{"visibility": "public", "body": "Reply", "as": "requester"}
		for k, v := range body {
			b[k] = v
		}
		return f.do("POST", tpath(key, "/messages"), b, bearer(tok))
	}
	statusName := func() any { return obj(f.get(f.dana.Access, key).expect(200).JSON(), "status")["name"] }

	// Resolve, then the teammate replies: authored as Sam, ticket reopens.
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	r := post(tok, map[string]any{"contactId": sam}).expect(201).JSON()
	if a := obj(r, "author"); a["id"] != sam || a["name"] != "Sam Teammate" || a["context"] != "contact" {
		t.Fatalf("teammate author %v", a)
	}
	if statusName() != "Open" {
		t.Fatalf("status after teammate reply: %v", statusName())
	}
	tk := f.get(f.dana.Access, key).expect(200).JSON()
	if tk["firstResponseAt"] != nil {
		t.Fatalf("a contact reply set the first response: %v", tk["firstResponseAt"])
	}
	if obj(tk, "requester")["id"] != priya {
		t.Fatalf("requester changed: %v", tk["requester"])
	}
	// The requester's own id, or no contactId, authors as the requester.
	for _, body := range []map[string]any{{"contactId": priya}, {}} {
		if a := obj(post(tok, body).expect(201).JSON(), "author"); a["id"] != priya {
			t.Fatalf("requester author %v", a)
		}
	}

	// A teammate reply ends a hold.
	f.hold(f.dana.Access, key, f.holdReasonID("Waiting on Engineering"), nil).expect(200)
	post(tok, map[string]any{"contactId": sam}).expect(201)
	if statusName() != "Open" {
		t.Fatalf("hold not ended: %v", statusName())
	}

	// Rejections.
	post(tok, map[string]any{"contactId": gus}).expectFieldCode(t, "contactNotInOrganisation")
	post(tok, map[string]any{"contactId": loner}).expectFieldCode(t, "contactNotInOrganisation")
	post(tok, map[string]any{"contactId": "000000000000000000000000"}).expectFieldCode(t, "contactNotInOrganisation")
	post(tok, map[string]any{"contactId": "bad"}).expect(422)
	post(both, map[string]any{"as": "staff", "contactId": sam}).expectFieldCode(t, "notAllowed")
	post(f.dana.Access, map[string]any{"as": "staff", "contactId": sam}).expectFieldCode(t, "notAllowed")
	post(f.dana.Access, map[string]any{"as": "requester", "contactId": sam}).expect(422)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "x", "contactId": sam}, bearer(both)).expectFieldCode(t, "notAllowed")

	// A ticket without an organisation never accepts another contact.
	plain := f.createTicket(f.integration, map[string]any{"requester": map[string]any{"contactId": priya}}).Ref()
	f.do("POST", tpath(plain, "/messages"), map[string]any{"visibility": "public", "body": "x", "as": "requester", "contactId": sam}, bearer(tok)).expectFieldCode(t, "contactNotInOrganisation")
	f.do("POST", tpath(plain, "/messages"), map[string]any{"visibility": "public", "body": "x", "as": "requester", "contactId": priya}, bearer(tok)).expect(201)
}

func TestMCPReplyAsOrganisationContact(t *testing.T) {
	f := newMCPFixture(t)
	acme := f.createOrg(f.admin.Access, "Acme Ltd", "")["id"].(string)
	mk := func(name, email string) string {
		r := f.do("POST", "/api/v1/contacts", map[string]any{"name": name, "email": email, "organisationId": acme}, bearer(f.admin.Access))
		return r.JSON()["id"].(string)
	}
	priya, sam := mk("Priya Raman", "priya@acme.example"), mk("Sam Teammate", "sam@acme.example")
	key := f.createTicket(f.ticketFixture.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya}}).Ref()
	_, tok := f.token("Gateway", "tickets:read", "tickets:replyAsRequester")
	s := f.connect(tok)
	m := mustCall(t, s, "ticketsReply", map[string]any{"ticket": key, "audience": "public", "body": "Hi", "as": "requester", "contactId": sam})["message"].(map[string]any)
	if a := obj(m, "author"); a["id"] != sam || a["name"] != "Sam Teammate" {
		t.Fatalf("mcp author %v", a)
	}
	_, both := f.token("Both", "tickets:read", "tickets:replyAsRequester", "tickets:comment")
	mustFail(t, f.connect(both), "ticketsReply", map[string]any{"ticket": key, "audience": "public", "body": "Hi", "as": "staff", "contactId": sam}, "validationFailed")
}
