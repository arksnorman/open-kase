package runtime

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Requester tokens cannot set staff-only fields through follow-up, and a
// refused copy duplicates no files.
func TestCopyAndFollowUpGuards(t *testing.T) {
	f := newTicketFixture(t)
	f.do("POST", "/api/v1/tags", map[string]any{"name": "vip"}, bearer(f.admin.Access)).expect(201)
	att := f.upload(f.integration, "a.png", []byte("\x89PNG\r\n\x1a\nbytes")).expect(201).Str("id")
	key := f.createTicket(f.integration, map[string]any{"attachmentIds": []string{att}}).Ref()

	f.do("POST", tpath(key, "/follow-up"), map[string]any{"tags": []string{"vip"}}, bearer(f.integration)).expect(403, "staffOnly")
	f.do("POST", tpath(key, "/follow-up"), map[string]any{"groupId": f.triage}, bearer(f.integration)).expect(403, "staffOnly")
	f.do("POST", tpath(key, "/follow-up"), map[string]any{"tags": []string{}}, bearer(f.integration)).expect(201)

	n := func() int64 {
		c, err := f.coll("attachments").CountDocuments(context.Background(), bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := n()
	f.do("POST", tpath(key, "/copy"), map[string]any{"tags": []string{"nope"}}, bearer(f.dana.Access)).expect(422)
	f.do("POST", tpath(key, "/copy"), map[string]any{"groupId": "000000000000000000000000"}, bearer(f.dana.Access)).expect(422)
	if after := n(); after != before {
		t.Fatalf("refused copies staged files: %d -> %d", before, after)
	}
	f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(f.dana.Access)).expect(201)
	if n() != before+1 {
		t.Fatal("a copy should add exactly one attachment record")
	}
}

func activityActions(f *ticketFixture, key string) []string {
	f.t.Helper()
	var out []string
	for _, it := range f.do("GET", tpath(key, "/activity"), nil, bearer(f.admin.Access)).expect(200).Items() {
		out = append(out, it["action"].(string))
	}
	return out
}

// Copying a ticket (also an archived one) copies the requester, CC,
// organisation, priority, request type, group, tags, first message and its
// files, links both ways and leaves assignee, watchers and notes behind.
func TestCopyTicket(t *testing.T) {
	f := newTicketFixture(t)
	f.do("POST", "/api/v1/tags", map[string]any{"name": "vip"}, bearer(f.admin.Access)).expect(201)
	att := f.upload(f.integration, "scan.png", []byte("\x89PNG\r\n\x1a\ncopy me")).expect(201).Str("id")
	orgID := f.createOrg(f.admin.Access, "Acme", "acme-1")["id"].(string)
	orig := f.createTicket(f.integration, map[string]any{"subject": "Bounced invoices", "priority": 2, "attachmentIds": []string{att},
		"organisationId": orgID, "ccEmails": []string{"cc@acme.example"}, "body": "Original request text"}).JSON()
	key := refOf(orig)
	f.do("PATCH", tpath(key), map[string]any{"tags": []string{"vip"}, "priority": 2}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	f.do("POST", tpath(key, "/claim"), nil, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	f.watch(f.marco.Access, key)
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "SECRET-NOTE"}, bearer(f.dana.Access)).expect(201)
	f.reply(f.dana.Access, key, "public", "A later public reply")

	cp := f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(f.dana.Access), header("Idempotency-Key", "cp-1")).expect(201)
	nt := cp.JSON()
	nk := refOf(nt)
	if nk == key || nt["subject"] != "Bounced invoices" || nt["priority"] != float64(2) || nt["assignee"] != nil ||
		obj(nt, "requester")["id"] != obj(orig, "requester")["id"] || obj(nt, "group")["id"] != obj(orig, "group")["id"] ||
		obj(nt, "requestType")["id"] != obj(orig, "requestType")["id"] || obj(nt, "status")["category"] != "open" ||
		obj(nt, "organisation")["id"] != orgID || cp.Header.Get("Location") == "" {
		t.Fatalf("copy: %v", nt)
	}
	if tags, _ := nt["tags"].([]any); len(tags) != 1 || tags[0] != "vip" {
		t.Fatalf("copy tags: %v", nt["tags"])
	}
	if cc, _ := nt["ccEmails"].([]any); len(cc) != 1 || cc[0] != "cc@acme.example" {
		t.Fatalf("copy cc: %v", nt["ccEmails"])
	}
	if again := f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(f.dana.Access), header("Idempotency-Key", "cp-1")).expect(201).Ref(); again != nk {
		t.Fatalf("replay created %s, want %s", again, nk)
	}
	msgs := f.do("GET", tpath(nk, "/messages"), nil, bearer(f.dana.Access)).expect(200)
	items := msgs.Items()
	if len(items) != 1 || items[0]["visibility"] != "public" || items[0]["body"] != "Original request text" || strings.Contains(string(msgs.Body), "SECRET-NOTE") {
		t.Fatalf("copy messages: %s", msgs.Body)
	}
	files, _ := items[0]["attachments"].([]any)
	if len(files) != 1 {
		t.Fatalf("copy attachments: %s", msgs.Body)
	}
	newAtt := files[0].(map[string]any)["id"].(string)
	if newAtt == att {
		t.Fatal("attachment record shared with the original")
	}
	f.attachmentDownload(f.dana.Access, newAtt).expect(200)
	if w := f.do("GET", tpath(nk, "/watchers"), nil, bearer(f.dana.Access)).expect(200); strings.Contains(string(w.Body), "marco") {
		t.Fatalf("watchers copied: %s", w.Body)
	}
	if got := linkRelations(f.do("GET", tpath(nk, "/links"), nil, bearer(f.dana.Access)).expect(200)); got[key] != "copiedFrom" {
		t.Fatalf("copy link: %v", got)
	}
	if got := linkRelations(f.do("GET", tpath(key, "/links"), nil, bearer(f.dana.Access)).expect(200)); got[nk] != "copiedTo" {
		t.Fatalf("original link: %v", got)
	}
	if acts := activityActions(f, nk); !contains(acts, "ticket.copied") {
		t.Fatalf("copy audit: %v", acts)
	}

	// Overrides, files opt-out, no signature on the copied message.
	f.setSignature(f.dana.Access, "-- Dana")
	cp2 := f.do("POST", tpath(key, "/copy"), map[string]any{"subject": "Second site", "priority": 4, "body": "Edited text", "tags": []string{}, "includeAttachments": false}, bearer(f.dana.Access)).expect(201).JSON()
	m2 := f.do("GET", tpath(refOf(cp2), "/messages"), nil, bearer(f.dana.Access)).expect(200).Items()
	if cp2["subject"] != "Second site" || cp2["priority"] != float64(4) || m2[0]["body"] != "Edited text" || len(m2[0]["attachments"].([]any)) != 0 {
		t.Fatalf("override copy: %v %v", cp2, m2)
	}
	f.do("POST", tpath(key, "/copy"), map[string]any{"tags": []string{"unknown-tag"}}, bearer(f.dana.Access)).expect(422)
	f.do("POST", tpath(key, "/copy"), map[string]any{"subject": ""}, bearer(f.dana.Access)).expect(422)

	// An archived original can be copied; it stays unchanged.
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200)
	arch := f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "archive"}, bearer(f.dana.Access), ifMatchV(f.version(key))).expect(200).JSON()
	f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(f.dana.Access)).expect(201)
	if o := f.get(f.dana.Access, key).expect(200).JSON(); o["version"] != arch["version"] {
		t.Fatalf("original changed: %v", o)
	}

	// Tokens need tickets:create and tickets:read plus a staff scope.
	_, bot := f.token("agent bot", "tickets:read", "tickets:create", "tickets:write")
	f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(bot)).expect(201)
	f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(f.readOnlyIntegraton)).expect(403)
	_, noCreate := f.token("no create", "tickets:read", "tickets:write")
	f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(noCreate)).expect(403, "insufficientScope")
	_, reqTok := f.token("requester", "tickets:read", "tickets:create")
	f.do("POST", tpath(key, "/copy"), map[string]any{}, bearer(reqTok)).expect(403, "staffOnly")

	// The follow-up takes the same optional group, request type and tags.
	fu := f.do("POST", tpath(key, "/follow-up"), map[string]any{"groupId": f.triage, "tags": []string{"vip"}}, bearer(f.dana.Access)).expect(201).JSON()
	if obj(fu, "group")["id"] != f.triage || len(fu["tags"].([]any)) != 1 {
		t.Fatalf("follow-up overrides: %v", fu)
	}
}
