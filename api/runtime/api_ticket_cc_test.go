package runtime

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/arksnorman/open-kase/api/internal/notifications"
)

func ccList(t *testing.T, ticket map[string]any) []string {
	t.Helper()
	raw, ok := ticket["ccEmails"].([]any)
	if !ok {
		t.Fatalf("ccEmails missing or not a list: %v", ticket["ccEmails"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// Owner request 2026-10-31: per-ticket CC addresses, set on create and
// edited by staff, validated, normalised and audited.
func TestTicketCCEmailsCreatePatchAndValidation(t *testing.T) {
	f := newTicketFixture(t)

	// Create: normalised (trimmed, lower case, deduplicated).
	created := f.createTicket(f.integration, map[string]any{
		"ccEmails": []string{" Ops@Acme.example ", "ops@acme.example", "lead@acme.example"}}).JSON()
	if got := ccList(t, created); !slices.Equal(got, []string{"ops@acme.example", "lead@acme.example"}) {
		t.Fatalf("created cc: %v", got)
	}
	ref := refOf(created)
	if got := ccList(t, f.createTicket(f.dana.Access, map[string]any{}).JSON()); len(got) != 0 {
		t.Fatalf("default cc: %v", got)
	}

	// Validation: format, the requester's own address, more than 10.
	base := func(cc []string) map[string]any {
		return map[string]any{"subject": "s", "body": "b", "requester": map[string]any{"name": "P", "email": "p@example.com"}, "ccEmails": cc}
	}
	r := f.do("POST", "/api/v1/tickets", base([]string{"not-an-email"}), bearer(f.dana.Access)).expect(422, "validationFailed")
	if fe := r.Problem().Errors; len(fe) != 1 || fe[0].Field != "ccEmails[0]" || fe[0].Code != "invalidFormat" {
		t.Fatalf("format error: %+v", fe)
	}
	r = f.do("POST", "/api/v1/tickets", base([]string{"x@example.com", "P@Example.com"}), bearer(f.dana.Access)).expect(422, "validationFailed")
	if fe := r.Problem().Errors; len(fe) != 1 || fe[0].Field != "ccEmails[1]" || fe[0].Code != "requesterEmail" {
		t.Fatalf("requester error: %+v", fe)
	}
	many := []string{}
	for _, c := range "abcdefghijk" {
		many = append(many, string(c)+"@example.com")
	}
	r = f.do("POST", "/api/v1/tickets", base(many), bearer(f.dana.Access)).expect(422, "validationFailed")
	if fe := r.Problem().Errors; len(fe) != 1 || fe[0].Field != "ccEmails" {
		t.Fatalf("count error: %+v", fe)
	}

	// PATCH by staff with If-Match; audited as ticket.updated (ccEmails).
	f.do("PATCH", tpath(ref), map[string]any{"ccEmails": []string{"a@example.com"}}, bearer(f.dana.Access)).expect(428)
	up := f.do("PATCH", tpath(ref), map[string]any{"ccEmails": []string{"New@Example.com"}}, bearer(f.dana.Access),
		ifMatchV(f.version(ref))).expect(200).JSON()
	if got := ccList(t, up); !slices.Equal(got, []string{"new@example.com"}) {
		t.Fatalf("patched cc: %v", got)
	}
	f.do("PATCH", tpath(ref), map[string]any{"ccEmails": []string{"priya@acme.example"}}, bearer(f.dana.Access),
		ifMatchV(f.version(ref))).expect(422, "validationFailed")
	f.do("PATCH", tpath(ref), map[string]any{"ccEmails": []string{"a@example.com"}}, bearer(f.dana.Access), ifMatchV(1)).expect(409)
	// A requester-side token cannot edit CC.
	f.do("PATCH", tpath(ref), map[string]any{"ccEmails": []string{}}, bearer(f.integration)).expect(403)
	evs, err := f.c.Audit.RecentAudit(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, e := range evs {
		if e.Action == "ticket.updated" && slices.Contains(e.Changes, "ccEmails") {
			audited = true
		}
	}
	if !audited {
		t.Fatal("no ticket.updated audit with ccEmails")
	}
	cleared := f.do("PATCH", tpath(ref), map[string]any{"ccEmails": []string{}}, bearer(f.dana.Access), ifMatchV(f.version(ref))).expect(200).JSON()
	if got := ccList(t, cleared); len(got) != 0 {
		t.Fatalf("cleared cc: %v", got)
	}
}

// Every requester email also goes to each CC address (its own delivery,
// same content); staff emails are unaffected.
func TestRequesterEmailsAreCopiedToCC(t *testing.T) {
	f := newNotifyFixture(t)
	f.createTicket(f.integration, map[string]any{"ccEmails": []string{"ops@acme.example"}})
	f.drain()
	f.reply(f.dana.Access, "1", "public", "SECRET-BODY-9 fixed")
	f.drain()
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.drain()

	req, cc := sentTo(f.mail, "priya@acme.example"), sentTo(f.mail, "ops@acme.example")
	if len(req) != 3 || len(cc) != len(req) {
		t.Fatalf("requester %d, cc %d", len(req), len(cc))
	}
	for i := range req {
		if req[i].Subject != cc[i].Subject || req[i].Headers["X-Open-Kase-Template"] != cc[i].Headers["X-Open-Kase-Template"] {
			t.Fatalf("cc email differs: %q vs %q", req[i].Subject, cc[i].Subject)
		}
		if req[i].MessageId == cc[i].MessageId {
			t.Fatal("cc email reuses the requester's Message-ID")
		}
		if strings.Contains(cc[i].Text+cc[i].HTML, "SECRET-BODY-9") {
			t.Fatal("cc email carries the message body")
		}
	}
	var ccRows int
	for _, d := range f.deliveries() {
		if d.Reason == notifications.ReasonCC {
			ccRows++
			if d.Recipient.Kind != notifications.RecipientAddress || d.Recipient.Address != "ops@acme.example" || d.Status != notifications.StatusSent {
				t.Fatalf("cc delivery: %+v", d)
			}
		}
	}
	if ccRows != 3 {
		t.Fatalf("cc delivery rows: %d", ccRows)
	}
	// Staff emails do not copy CC: the address got only the 3 requester emails.
	for _, m := range f.mail.Sent() {
		if m.To.Email == "ops@acme.example" && m.Headers["X-Open-Kase-Template"] == "publicReply.v1" {
			t.Fatal("cc address received a staff email")
		}
	}

	// Removed from CC before sending: the planned delivery is skipped.
	f.mail.Reset()
	f.do("PATCH", tpath("1"), map[string]any{"ccEmails": []string{}}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "reopen"}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.reply(f.dana.Access, "1", "public", "again")
	f.drain()
	if len(sentTo(f.mail, "ops@acme.example")) != 0 || len(sentTo(f.mail, "priya@acme.example")) != 1 {
		t.Fatalf("after removing cc: %+v", f.mail.Sent())
	}
}
