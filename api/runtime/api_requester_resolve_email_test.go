package runtime

import "testing"

func TestRequesterSideResolveSendsNoRequesterEmail(t *testing.T) {
	f := newNotifyFixture(t)
	_, tok := f.token("Portal", "tickets:read", "tickets:replyAsRequester")
	own := f.createTicket(f.integration, map[string]any{"subject": "Closed by customer", "ccEmails": []string{"ops@acme.example"}}).Ref()
	f.drain()
	f.mail.Reset()
	f.do("POST", tpath(own, "/transitions"), map[string]any{"action": "resolve"}, bearer(tok)).expect(200)
	f.drain()
	for _, d := range f.deliveriesFor("ticket.statusChanged") {
		if d.Channel == "email" && (d.Recipient.Kind == "contact" || d.Recipient.Kind == "address") {
			t.Fatalf("requester-side resolve planned a requester delivery: %+v", d)
		}
	}
	if len(sentTo(f.mail, "priya@acme.example")) != 0 || len(sentTo(f.mail, "ops@acme.example")) != 0 {
		t.Fatalf("requester emailed about their own resolve: %+v", f.mail.Sent())
	}

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
