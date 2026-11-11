package runtime

import (
	"strings"
	"testing"

	"github.com/arksnorman/open-kase/api/internal/outbox"
)

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// A requester-side resolve emails the CC addresses, not the requester who did it.
func TestRequesterSideResolveEmailsCCNotRequester(t *testing.T) {
	f := newNotifyFixture(t)
	_, tok := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	own := f.createTicket(f.integration, map[string]any{"subject": "Closed by customer", "ccEmails": []string{"ops@acme.example"}}).Ref()
	f.drain()
	f.mail.Reset()
	f.do("POST", tpath(own, "/transitions"), map[string]any{"action": "resolve"}, bearer(tok)).expect(200)
	f.drain()
	if len(sentTo(f.mail, "priya@acme.example")) != 0 {
		t.Fatalf("requester emailed about their own resolve: %+v", f.mail.Sent())
	}
	if got := sentTo(f.mail, "ops@acme.example"); len(got) != 1 {
		t.Fatalf("CC address not told about the resolve: %+v", f.mail.Sent())
	}

	// With requester emails off the CC delivery is recorded as skipped and nothing is sent.
	put, v := f.settingsPut(map[string]any{"notifications": map[string]any{"requesterEmails": false}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	off := f.createTicket(f.integration, map[string]any{"subject": "Closed while off", "ccEmails": []string{"ops@acme.example"}}).Ref()
	f.drain()
	f.mail.Reset()
	f.do("POST", tpath(off, "/transitions"), map[string]any{"action": "resolve"}, bearer(tok)).expect(200)
	f.drain()
	if len(sentTo(f.mail, "ops@acme.example")) != 0 || len(sentTo(f.mail, "priya@acme.example")) != 0 {
		t.Fatalf("customer side emailed while off: %+v", f.mail.Sent())
	}
	skipped := 0
	for _, d := range f.deliveriesFor("ticket.statusChanged") {
		if d.Channel == "email" && d.Recipient.Kind == "address" && d.Recipient.Address == "ops@acme.example" && d.Status == "skipped" && d.ErrorClass == "requesterEmailsOff" {
			skipped++
		}
		if d.Channel == "email" && d.Recipient.Kind == "contact" {
			t.Fatalf("requester delivery for their own resolve: %+v", d)
		}
	}
	if skipped == 0 {
		t.Fatalf("no skipped requesterEmailsOff delivery: %+v", f.deliveriesFor("ticket.statusChanged"))
	}
	put, v = f.settingsPut(map[string]any{"notifications": map[string]any{"requesterEmails": true}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200)

	// A staff resolve still tells the requester and the CC addresses.
	other := f.createTicket(f.integration, map[string]any{"subject": "Closed by staff", "ccEmails": []string{"ops@acme.example"}}).Ref()
	f.drain()
	f.mail.Reset()
	f.do("POST", tpath(other, "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version(other))).expect(200)
	f.drain()
	if len(sentTo(f.mail, "priya@acme.example")) != 1 || len(sentTo(f.mail, "ops@acme.example")) != 1 {
		t.Fatalf("staff resolve emails: %+v", f.mail.Sent())
	}
}

// A teammate's resolve or reply emails the requester and the CC addresses
// except the teammate's own; a requester's reply emails the CC addresses only.
func TestCustomerSideActionsEmailTheRestOfTheCustomerSide(t *testing.T) {
	f := newNotifyFixture(t)
	acme, priya, sam, _, _ := f.orgContacts()
	_, tok := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	newTicket := func() string {
		t.Helper()
		key := f.createTicket(f.integration, map[string]any{"organisationId": acme, "requester": map[string]any{"contactId": priya},
			"ccEmails": []string{"ops@acme.example", "sam@acme.example"}}).Ref()
		f.drain()
		f.mail.Reset()
		return key
	}
	reply := func(key string, body map[string]any) {
		t.Helper()
		body["visibility"], body["body"], body["as"] = "public", "SECRET-BODY from the customer side", "requester"
		f.do("POST", tpath(key, "/messages"), body, bearer(tok)).expect(201)
		f.drain()
	}
	noBodies := func() {
		t.Helper()
		for _, m := range f.mail.Sent() {
			if (m.To.Email == "priya@acme.example" || m.To.Email == "ops@acme.example" || m.To.Email == "sam@acme.example") &&
				(containsAny(m.Text+m.HTML, "SECRET-BODY")) {
				t.Fatalf("message body in an email to the customer side: %+v", m)
			}
		}
	}
	count := func(want map[string]int) {
		t.Helper()
		for _, a := range []string{"priya@acme.example", "ops@acme.example", "sam@acme.example"} {
			if got := len(sentTo(f.mail, a)); got != want[a] {
				t.Fatalf("emails to %s: %d, want %d: %+v", a, got, want[a], f.mail.Sent())
			}
		}
		noBodies()
	}

	// The requester replies: the CC addresses only.
	key := newTicket()
	reply(key, map[string]any{})
	count(map[string]int{"ops@acme.example": 1, "sam@acme.example": 1})
	if m := sentTo(f.mail, "ops@acme.example")[0]; !containsAny(m.Text, "There is a new reply on ticket") {
		t.Fatalf("reply notice: %+v", m)
	}

	// A teammate replies: the requester and the other CC addresses, not the teammate's address.
	key = newTicket()
	reply(key, map[string]any{"contactId": sam})
	count(map[string]int{"priya@acme.example": 1, "ops@acme.example": 1})

	// The requester resolves: the CC addresses only.
	key = newTicket()
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve"}, bearer(tok)).expect(200)
	f.drain()
	count(map[string]int{"ops@acme.example": 1, "sam@acme.example": 1})

	// A teammate resolves: the requester and the other CC addresses.
	key = newTicket()
	f.do("POST", tpath(key, "/transitions"), map[string]any{"action": "resolve", "contactId": sam}, bearer(tok)).expect(200)
	f.drain()
	count(map[string]int{"priya@acme.example": 1, "ops@acme.example": 1})

	// A teammate's reply that reopens the ticket sends no status email, only the reply notice.
	f.mail.Reset()
	reply(key, map[string]any{"contactId": sam})
	count(map[string]int{"priya@acme.example": 1, "ops@acme.example": 1})
	for _, m := range f.mail.Sent() {
		if m.To.Email == "ops@acme.example" && containsAny(m.Subject, "resolved", "reopened") {
			t.Fatalf("status email for a requester reply: %+v", m)
		}
	}

	// An agent note never reaches the customer side.
	f.mail.Reset()
	f.reply(f.dana.Access, key, "agent", "SECRET-BODY internal")
	f.drain()
	count(nil)

	// With requester emails off the deliveries are skipped, nothing is sent.
	put, v := f.settingsPut(map[string]any{"notifications": map[string]any{"requesterEmails": false}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	key = newTicket()
	reply(key, map[string]any{"contactId": sam})
	count(nil)
	skipped := map[string]int{}
	for _, d := range f.deliveriesFor(outbox.MessageCreated) {
		if d.TicketId == f.ticketId(key) && d.Channel == "email" && (d.Recipient.Kind == "contact" || d.Recipient.Kind == "address") {
			if d.Status != "skipped" || d.ErrorClass != "requesterEmailsOff" {
				t.Fatalf("customer-side delivery while off: %+v", d)
			}
			skipped[d.Recipient.Kind]++
		}
	}
	if skipped["contact"] == 0 || skipped["address"] == 0 {
		t.Fatalf("skipped deliveries: %v", skipped)
	}
}
