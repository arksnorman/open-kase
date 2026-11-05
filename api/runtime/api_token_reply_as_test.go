package runtime

import (
	"slices"
	"testing"
)

// TestReplyAsRequesterOrStaff: who authors a token's message. With only
// tickets:replyAsRequester the requester does (no first response); with
// tickets:comment the token does as a bot (a staff reply that meets the first
// response target); with both, as picks the author (default staff). Users
// always post as themselves. The audit names the scope that allowed it.
func TestReplyAsRequesterOrStaff(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	key := f.createTicket(f.dana.Access, map[string]any{"priority": 1}).Ref()
	post := func(tok string, body map[string]any) result {
		t.Helper()
		return f.do("POST", tpath(key, "/messages"), body, bearer(tok))
	}
	pub := func(extra ...string) map[string]any {
		b := map[string]any{"visibility": "public", "body": "Reply"}
		if len(extra) == 2 {
			b[extra[0]] = extra[1]
		}
		return b
	}
	authorContext := func(r result) any { t.Helper(); return obj(r.JSON(), "author")["context"] }
	firstResponseMet := func() bool {
		t.Helper()
		tk := f.get(f.dana.Access, key).expect(200).JSON()
		fr := target(slaOf(tk), "firstResponse")
		return tk["firstResponseAt"] != nil && fr != nil && fr["state"] == "met"
	}

	// Only tickets:replyAsRequester: the requester authors the reply.
	reqID, req := f.token("Gateway", "tickets:read", "tickets:replyAsRequester")
	if c := authorContext(post(req, pub()).expect(201)); c != "contact" {
		t.Fatalf("replyAsRequester author %v", c)
	}
	if c := authorContext(post(req, pub("as", "requester")).expect(201)); c != "contact" {
		t.Fatalf("explicit requester author %v", c)
	}
	if firstResponseMet() {
		t.Fatal("a requester reply met the first response target")
	}
	post(req, pub("as", "staff")).expect(403, "insufficientScope")
	post(req, map[string]any{"visibility": "agent", "body": "x"}).expect(403, "insufficientScope")
	n := 0
	for _, ev := range f.tokenAudit(reqID) {
		if ev.Action == "message.created" {
			n++
			if !slices.Equal(ev.ActorScopes, []string{"tickets:replyAsRequester"}) {
				t.Fatalf("requester reply audit scopes %v", ev.ActorScopes)
			}
		}
	}
	if n != 2 {
		t.Fatalf("requester reply audit events: %d", n)
	}

	// Only tickets:comment: the bot authors it and meets the first response.
	botID, bot := f.token("Bot", "tickets:read", "tickets:comment")
	post(bot, pub("as", "requester")).expect(403, "insufficientScope")
	if a := obj(post(bot, pub()).expect(201).JSON(), "author"); a["context"] != "bot" || a["id"] != botID || a["name"] != "Bot" {
		t.Fatalf("staff reply author %v", a)
	}
	if !firstResponseMet() {
		t.Fatal("a staff token reply did not meet the first response target")
	}
	for _, ev := range f.tokenAudit(botID) {
		if ev.Action == "message.created" && !slices.Equal(ev.ActorScopes, []string{"tickets:comment"}) {
			t.Fatalf("staff reply audit scopes %v", ev.ActorScopes)
		}
	}

	// Both reply scopes: as decides, staff by default.
	_, both := f.token("Both", "tickets:read", "tickets:replyAsRequester", "tickets:comment", "tickets:internal", "tickets:write")
	if c := authorContext(post(both, pub()).expect(201)); c != "bot" {
		t.Fatalf("default author with both scopes %v", c)
	}
	if c := authorContext(post(both, pub("as", "staff")).expect(201)); c != "bot" {
		t.Fatalf("as staff author %v", c)
	}
	if c := authorContext(post(both, pub("as", "requester")).expect(201)); c != "contact" {
		t.Fatalf("as requester author %v", c)
	}
	if c := authorContext(post(both, map[string]any{"visibility": "agent", "body": "Note"}).expect(201)); c != "bot" {
		t.Fatalf("note author %v", c)
	}
	post(both, map[string]any{"visibility": "agent", "body": "x", "as": "requester"}).expect(422, "validationFailed")
	post(both, map[string]any{"visibility": "public", "body": "x", "as": "requester", "thenStatusId": f.statusID("resolved")}).expect(422, "validationFailed")
	post(both, pub("as", "customer")).expect(422, "validationFailed")

	// Users always post as themselves.
	post(f.dana.Access, pub("as", "requester")).expect(422, "validationFailed")
	if c := authorContext(post(f.dana.Access, pub("as", "staff")).expect(201)); c != "staff" {
		t.Fatalf("user author %v", c)
	}
}

// TestMCPReplyAs: ticketsReply applies the same author rule as REST.
func TestMCPReplyAs(t *testing.T) {
	f := newMCPFixture(t)
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	_, both := f.token("Both", "tickets:read", "tickets:replyAsRequester", "tickets:comment")
	s := f.connect(both)
	author := func(args map[string]any) any {
		t.Helper()
		return obj(mustCall(t, s, "ticketsReply", args)["message"].(map[string]any), "author")["context"]
	}
	if c := author(map[string]any{"ticket": key, "audience": "public", "body": "Hi"}); c != "bot" {
		t.Fatalf("default author %v", c)
	}
	if c := author(map[string]any{"ticket": key, "audience": "public", "body": "Hi", "as": "requester"}); c != "contact" {
		t.Fatalf("as requester author %v", c)
	}
	_, staff := f.token("Staff only", "tickets:read", "tickets:comment")
	mustFail(t, f.connect(staff), "ticketsReply", map[string]any{"ticket": key, "audience": "public", "body": "Hi", "as": "requester"}, "insufficientScope")
}
