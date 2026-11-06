package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// hookEnvelopes decodes the webhook deliveries received so far, by event type.
func hookEnvelopes(t *testing.T, hs *hookServer) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, r := range hs.all() {
		var env map[string]any
		if err := json.Unmarshal(r.body, &env); err != nil {
			t.Fatal(err)
		}
		typ, _ := env["type"].(string)
		out[typ] = append(out[typ], env)
	}
	return out
}

func (f *ticketFixture) addHook(hs *hookServer, types ...string) {
	f.t.Helper()
	f.do("POST", "/api/v1/webhooks", map[string]any{"name": "Test hook", "url": hs.URL + "/hook", "eventTypes": types},
		bearer(f.admin.Access)).expect(201)
}

func TestRequesterEmailsSwitchSkipsContactsAndCC(t *testing.T) {
	f := newNotifyFixture(t)
	if cur := f.do("GET", "/api/v1/settings", nil, bearer(f.admin.Access)).expect(200).JSON(); obj(cur, "notifications")["requesterEmails"] != true {
		t.Fatalf("default: %v", cur["notifications"])
	}
	// False turns it off (admins only); an absent field keeps the stored value.
	put, v := f.settingsPut(map[string]any{"notifications": map[string]any{"requesterEmails": false}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.dana.Access), ifMatchV(v)).expect(403)
	got := f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON()
	if obj(got, "notifications")["requesterEmails"] != false {
		t.Fatalf("not turned off: %v", got)
	}
	put, v = f.settingsPut(map[string]any{"notifications": map[string]any{"requesterLinkBaseUrl": ""}})
	got = f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON()
	if obj(got, "notifications")["requesterEmails"] != false {
		t.Fatalf("absent field reset the switch: %v", got)
	}

	f.createTicket(f.integration, map[string]any{"ccEmails": []string{"ops@acme.example"}})
	f.reply(f.dana.Access, "1", "public", "hello")
	f.drain()
	if len(sentTo(f.mail, "priya@acme.example")) != 0 || len(sentTo(f.mail, "ops@acme.example")) != 0 {
		t.Fatalf("requester emails sent while off: %+v", f.mail.Sent())
	}
	skippedKinds := map[string]int{}
	staffPlanned := 0
	for _, d := range f.deliveries() {
		if d.Channel != "email" {
			continue
		}
		switch d.Recipient.Kind {
		case "contact", "address":
			if d.Status != "skipped" || d.ErrorClass != "requesterEmailsOff" {
				t.Fatalf("requester delivery: %+v", d)
			}
			skippedKinds[d.Recipient.Kind]++
		default:
			staffPlanned++
		}
	}
	if skippedKinds["contact"] == 0 || skippedKinds["address"] == 0 || staffPlanned == 0 {
		t.Fatalf("skipped=%v staff=%d", skippedKinds, staffPlanned)
	}

	// Back on: requesters are emailed again.
	put, v = f.settingsPut(map[string]any{"notifications": map[string]any{"requesterEmails": true}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	f.mail.Reset()
	f.reply(f.dana.Access, "1", "public", "again")
	f.drain()
	if len(sentTo(f.mail, "priya@acme.example")) != 1 || len(sentTo(f.mail, "ops@acme.example")) != 1 {
		t.Fatalf("requester emails after turning on: %+v", f.mail.Sent())
	}
}

func TestWebhookRequesterContextOnReplyAndStatus(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	f.addHook(hs, "ticket.created", "ticket.message.public", "ticket.statusChanged")
	f.createTicket(f.integration, map[string]any{"subject": "Router down", "ccEmails": []string{"ops@acme.example"}})
	f.reply(f.dana.Access, "1", "public", "PUBLIC-REPLY")
	f.reply(f.dana.Access, "1", "agent", "AGENT-NOTE-BODY")
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.drain()
	by := hookEnvelopes(t, hs)
	var created map[string]any
	for _, typ := range []string{"ticket.created", "ticket.message.public", "ticket.statusChanged"} {
		if len(by[typ]) == 0 {
			t.Fatalf("no %s delivery", typ)
		}
		d := obj(by[typ][0], "data")
		if typ == "ticket.created" {
			created = d
		}
		req := obj(d, "requester")
		if req["email"] != "priya@acme.example" || req["name"] != "Priya Raman" || req["id"] == "" {
			t.Fatalf("%s requester: %v", typ, d)
		}
		if _, ok := req["externalId"]; !ok {
			t.Fatalf("%s requester externalId missing: %v", typ, req)
		}
		if _, ok := d["organisation"]; !ok {
			t.Fatalf("%s organisation missing: %v", typ, d)
		}
		if d["subject"] != "Router down" {
			t.Fatalf("%s subject: %v", typ, d["subject"])
		}
		cc, _ := d["ccEmails"].([]any)
		if len(cc) != 1 || cc[0] != "ops@acme.example" {
			t.Fatalf("%s ccEmails: %v", typ, d["ccEmails"])
		}
	}
	a, _ := json.Marshal(created["requester"])
	for _, typ := range []string{"ticket.message.public", "ticket.statusChanged"} {
		b, _ := json.Marshal(obj(by[typ][0], "data")["requester"])
		if string(a) != string(b) {
			t.Fatalf("%s requester shape differs: %s vs %s", typ, a, b)
		}
	}
	for _, r := range hs.all() {
		if strings.Contains(string(r.body), "AGENT-NOTE-BODY") {
			t.Fatalf("agent note leaked: %s", r.body)
		}
	}
}

func TestFollowupReminderWebhook(t *testing.T) {
	f := newAutomationFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	f.addHook(hs, "ticket.followupReminder")
	f.do("PUT", "/api/v1/automation", map[string]any{"archiveAfterResolvedDays": 3, "customerFollowupAfterHours": 24, "maxReminders": 2,
		"reminderCooldownHours": 24, "autoResolveAfterHours": 48}, bearer(f.admin.Access), header("If-Match", "0")).expect(200)
	k := f.createTicket(f.integration, map[string]any{"subject": "Waiting one", "ccEmails": []string{"ops@acme.example"}}).Ref()
	f.waiting(f.dana.Access, k)
	f.advance(25 * time.Hour)
	f.advance(25 * time.Hour)
	f.runDue()
	got := hookEnvelopes(t, hs)["ticket.followupReminder"]
	if len(got) != 2 {
		t.Fatalf("reminder webhooks: %d (%v)", len(got), hs.all())
	}
	for i, env := range got {
		d := obj(env, "data")
		want := i == 1
		if d["autoResolveNotice"] != want {
			t.Fatalf("reminder %d autoResolveNotice: %v", i+1, d)
		}
		if obj(d, "requester")["email"] != "priya@acme.example" || d["subject"] != "Waiting one" {
			t.Fatalf("reminder payload: %v", d)
		}
		if cc, _ := d["ccEmails"].([]any); len(cc) != 1 {
			t.Fatalf("ccEmails: %v", d)
		}
	}
}
