package runtime

import (
	"strings"
	"testing"
	"time"
)

func (f *ticketFixture) putWorkspaceSignature(sig string) result {
	f.t.Helper()
	cur := f.do("GET", "/api/v1/settings", nil, bearer(f.admin.Access)).expect(200)
	doc := cur.JSON()
	version := jsonString(doc["configVersion"])
	delete(doc, "configVersion")
	if smtp, ok := doc["smtp"].(map[string]any); ok {
		delete(smtp, "passwordConfigured")
	}
	doc["product"].(map[string]any)["signature"] = sig
	return f.do("PUT", "/api/v1/settings", doc, bearer(f.admin.Access), header("If-Match", version))
}

func (f *ticketFixture) setWorkspaceSignature(sig string) {
	f.t.Helper()
	f.putWorkspaceSignature(sig).expect(200)
	if _, err := f.c.Settings.Refresh(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ticketFixture) setSignature(access, sig string) {
	f.t.Helper()
	f.do("PUT", "/api/v1/me/preferences", map[string]any{"signature": sig}, bearer(access)).expect(200)
}

func TestReplySignatures(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	key := f.createTicket(f.integration, map[string]any{"subject": "Signed replies"}).Ref()
	post := func(tok string, body map[string]any) string {
		t.Helper()
		return f.do("POST", tpath(key, "/messages"), body, bearer(tok)).expect(201).Str("body")
	}
	pub := func(tok, text string, extra ...any) string {
		b := map[string]any{"visibility": "public", "body": text}
		for i := 0; i+1 < len(extra); i += 2 {
			b[extra[i].(string)] = extra[i+1]
		}
		return post(tok, b)
	}

	// Neither signature set: the body is unchanged.
	if got := pub(f.dana.Access, "Plain"); got != "Plain" {
		t.Fatalf("no signatures: %q", got)
	}

	// Only the workspace signature.
	f.setWorkspaceSignature("Acme Support")
	if got := pub(f.dana.Access, "Hello"); got != "Hello\n\nAcme Support" {
		t.Fatalf("workspace only: %q", got)
	}
	f.setWorkspaceSignature("")

	// Only the personal signature.
	f.setSignature(f.dana.Access, "Dana\nSupport engineer")
	if got := pub(f.dana.Access, "Hello"); got != "Hello\n\nDana\nSupport engineer" {
		t.Fatalf("personal only: %q", got)
	}
	// Another agent has no signature.
	if got := pub(f.marco.Access, "Hi"); got != "Hi" {
		t.Fatalf("other agent: %q", got)
	}

	// Both: personal first, then workspace.
	f.setWorkspaceSignature("Acme Support")
	if got := pub(f.dana.Access, "Hello"); got != "Hello\n\nDana\nSupport engineer\n\nAcme Support" {
		t.Fatalf("both: %q", got)
	}
	// Reply with status takes the same path.
	st := f.statusIDs()
	if got := pub(f.dana.Access, "Done", "thenStatusId", st["waitingCustomer"]); !strings.HasSuffix(got, "\n\nAcme Support") {
		t.Fatalf("reply with status: %q", got)
	}
	// addSignature:false skips both.
	if got := pub(f.dana.Access, "Bare", "addSignature", false); got != "Bare" {
		t.Fatalf("addSignature=false: %q", got)
	}
	// Never on agent notes.
	if got := post(f.dana.Access, map[string]any{"visibility": "agent", "body": "Note"}); got != "Note" {
		t.Fatalf("agent note: %q", got)
	}
	// Never on integration tokens (bot or requester).
	_, bot := f.token("Bot", "tickets:read", "tickets:comment")
	if got := pub(bot, "From bot"); got != "From bot" {
		t.Fatalf("token as staff: %q", got)
	}
	_, req := f.token("Gateway", "tickets:read", "tickets:replyAsRequester")
	if got := pub(req, "From requester"); got != "From requester" {
		t.Fatalf("requester message: %q", got)
	}

	// The profile returns it; the limits are enforced.
	if r := f.do("GET", "/api/v1/me", nil, bearer(f.dana.Access)).expect(200); r.Str("signature") != "Dana\nSupport engineer" {
		t.Fatalf("me.signature %q", r.Str("signature"))
	}
	long := strings.Repeat("x", 1001)
	f.do("PUT", "/api/v1/me/preferences", map[string]any{"signature": long}, bearer(f.dana.Access)).expect(422)
	f.setSignature(f.dana.Access, strings.Repeat("x", 1000))
	f.putWorkspaceSignature(long).expect(422)
	f.putWorkspaceSignature(strings.Repeat("y", 1000)).expect(200)
}

func TestScheduledSendSignsAtSendTime(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	f.setSignature(f.dana.Access, "Dana v1")
	key := f.createTicket(f.integration, map[string]any{"subject": "Scheduled signed"}).Ref()
	f.runDue()
	at := func(h int) string { return f.clk.Now().Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }

	signed := f.schedule(f.dana.Access, key, map[string]any{"visibility": "public", "body": "Signed later", "sendAt": at(1)}).expect(201).JSON()
	unsigned := f.schedule(f.dana.Access, key, map[string]any{"visibility": "public", "body": "Bare later", "sendAt": at(1), "addSignature": false}).expect(201).JSON()
	f.schedule(f.dana.Access, key, map[string]any{"visibility": "agent", "body": "Note later", "sendAt": at(1)}).expect(201)
	if signed["addSignature"] != true || unsigned["addSignature"] != false {
		t.Fatalf("flag not stored: %v %v", signed["addSignature"], unsigned["addSignature"])
	}
	// The stored scheduled text never carries the signature.
	if signed["body"] != "Signed later" {
		t.Fatalf("scheduled body: %v", signed["body"])
	}

	// The author's signature changes before sending: the new one is used.
	f.setSignature(f.dana.Access, "Dana v2")
	f.setWorkspaceSignature("Acme")
	f.clk.Advance(2 * time.Hour)
	f.runDue()
	bodies := map[string]bool{}
	for _, m := range f.do("GET", tpath(key, "/messages?limit=100"), nil, bearer(f.admin.Access)).expect(200).Items() {
		bodies[m["body"].(string)] = true
	}
	for _, want := range []string{"Signed later\n\nDana v2\n\nAcme", "Bare later", "Note later"} {
		if !bodies[want] {
			t.Fatalf("missing %q in %v", want, bodies)
		}
	}
}
