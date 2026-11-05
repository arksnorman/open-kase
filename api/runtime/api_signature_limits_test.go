package runtime

import (
	"strings"
	"testing"
	"time"
)

func TestSignatureLimitsFencesAndFollowUp(t *testing.T) {
	f := newSLAFixture(t)
	f.putPolicy()
	key := f.createTicket(f.integration, map[string]any{"subject": "Signature limits"}).Ref()
	f.setSignature(f.dana.Access, "Dana")
	f.setWorkspaceSignature(strings.Repeat("w", 1000))
	long := strings.Repeat("a", 99_500) // fits alone, not with the signatures

	// Too long once signed: a clean 422 on the body, nothing posted.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": long}, bearer(f.dana.Access)).expect(422)
	f.do("POST", tpath(key, "/follow-up"), map[string]any{"body": long}, bearer(f.dana.Access)).expect(422)
	if n := f.messageCount(key); n != 1 {
		t.Fatalf("a rejected reply was posted: %d messages", n)
	}
	// Skipping the signature makes it fit.
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": long, "addSignature": false}, bearer(f.dana.Access)).expect(201)
	f.do("POST", tpath(key, "/follow-up"), map[string]any{"body": "Follow up text", "addSignature": false}, bearer(f.dana.Access)).expect(201)

	// An unclosed code fence is closed before the signatures.
	f.setWorkspaceSignature("Acme")
	got := f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "Run:\n```sh\nmake"}, bearer(f.dana.Access)).expect(201).Str("body")
	if got != "Run:\n```sh\nmake\n```\n\nDana\n\nAcme" {
		t.Fatalf("fence: %q", got)
	}
	// A closed fence stays untouched.
	got = f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "public", "body": "```\nx\n```"}, bearer(f.dana.Access)).expect(201).Str("body")
	if got != "```\nx\n```\n\nDana\n\nAcme" {
		t.Fatalf("closed fence: %q", got)
	}

	// A scheduled message that no longer fits once signed is held for review.
	f.setWorkspaceSignature("")
	f.setSignature(f.dana.Access, "")
	at := f.clk.Now().Add(time.Hour).Format(time.RFC3339)
	sm := f.schedule(f.dana.Access, key, map[string]any{"visibility": "public", "body": long, "sendAt": at}).expect(201).JSON()
	f.setWorkspaceSignature(strings.Repeat("w", 1000))
	f.clk.Advance(2 * time.Hour)
	f.runDue()
	held := f.scheduledByID(key, sm["id"].(string))
	if held["state"] != "needsReview" || held["reason"] != "invalidMessage" {
		t.Fatalf("scheduled too long once signed: %v / %v", held["state"], held["reason"])
	}
	// The flag can be edited.
	r := f.do("PATCH", spath(key, "/"+sm["id"].(string)), map[string]any{"addSignature": false, "sendAt": f.clk.Now().Add(time.Hour).Format(time.RFC3339)}, bearer(f.dana.Access),
		header("If-Match", jsonString(held["version"]))).expect(200)
	if r.JSON()["addSignature"] != false {
		t.Fatalf("flag not updated: %v", r.JSON()["addSignature"])
	}
}
