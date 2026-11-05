package runtime

import (
	"strings"
	"testing"
)

// settingsPut builds a PUT /settings body from the current settings plus
// extra top-level sections, and returns the current config version.
func (f *ticketFixture) settingsPut(extra map[string]any) (map[string]any, int64) {
	f.t.Helper()
	cur := f.do("GET", "/api/v1/settings", nil, bearer(f.admin.Access)).expect(200).JSON()
	smtp := obj(cur, "smtp")
	delete(smtp, "passwordConfigured")
	put := map[string]any{"product": cur["product"], "auth": cur["auth"], "files": cur["files"], "smtp": smtp}
	for k, v := range extra {
		put[k] = v
	}
	return put, int64(cur["configVersion"].(float64))
}

// Owner decision 2026-10-31: requester emails never carry message content
// and link only to the configured requesting system.
func TestRequesterEmailsCarryNoContentAndNoAgentLink(t *testing.T) {
	f := newNotifyFixture(t)
	f.createTicket(f.integration, map[string]any{})
	f.drain()
	f.mail.Reset()
	f.reply(f.dana.Access, "1", "public", "SECRET-BODY-42 the fix is in production")
	f.drain()
	got := sentTo(f.mail, "priya@acme.example")
	if len(got) != 1 {
		t.Fatalf("requester emails: %+v", got)
	}
	m := got[0]
	all := m.Subject + m.Text + m.HTML
	if strings.Contains(all, "SECRET-BODY-42") || strings.Contains(all, "Dana") {
		t.Fatalf("requester email carries message content or author: %+v", m)
	}
	if strings.Contains(all, "support.example.com") || strings.Contains(all, "href=") || strings.Contains(all, "https://") {
		t.Fatalf("requester email has a link without a configured requester link: %+v", m)
	}
	for _, body := range []string{m.Text, m.HTML} {
		if !strings.Contains(body, "#1") || !strings.Contains(body, "open the system you used to submit it") {
			t.Fatalf("missing guidance: %s", body)
		}
	}
	if m.Headers["X-Open-Kase-Template"] != "requesterReply.v1" {
		t.Fatalf("template: %v", m.Headers)
	}
}

func TestRequesterLinkSettingAndEmails(t *testing.T) {
	f := newNotifyFixture(t)
	const field = "notifications.requesterLinkBaseUrl"
	for _, bad := range []string{"ftp://portal.example.org", "portal.example.org/requests", "https://portal.example.org/r?x=1",
		"https://portal.example.org/r#top", "https://support.example.com/portal", "https://user:pw@portal.example.org"} {
		put, v := f.settingsPut(map[string]any{"notifications": map[string]any{"requesterLinkBaseUrl": bad}})
		r := f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(422)
		if !strings.Contains(string(r.Body), field) {
			t.Fatalf("%s: %s", bad, r.Body)
		}
	}
	put, v := f.settingsPut(map[string]any{"notifications": map[string]any{"requesterLinkBaseUrl": "https://portal.example.org/requests/"}})
	f.do("PUT", "/api/v1/settings", put, bearer(f.dana.Access), ifMatchV(v)).expect(403)
	got := f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON()
	if obj(got, "notifications")["requesterLinkBaseUrl"] != "https://portal.example.org/requests/" {
		t.Fatalf("settings: %v", got)
	}
	// An older client that omits the section keeps the stored value.
	put, v = f.settingsPut(nil)
	got = f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON()
	if obj(got, "notifications")["requesterLinkBaseUrl"] != "https://portal.example.org/requests/" {
		t.Fatalf("absent section cleared the link: %v", got)
	}
	assertAudit(t, f.harness, "settings.updated")

	// Acknowledgement, reply notice and resolve notice all link to
	// <base>/<key>, never to the agent UI, and carry no message content.
	const link = "https://portal.example.org/requests/1"
	f.createTicket(f.integration, map[string]any{})
	f.reply(f.dana.Access, "1", "public", "SECRET-BODY-77 please restart the router")
	f.drain()
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve"}, bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.drain()
	sent := sentTo(f.mail, "priya@acme.example")
	if len(sent) != 3 {
		t.Fatalf("requester emails: %d %+v", len(sent), sent)
	}
	for _, m := range sent {
		all := m.Subject + m.Text + m.HTML
		if !strings.Contains(m.Text, link) || !strings.Contains(m.HTML, `href="`+link+`"`) {
			t.Fatalf("requester link missing (%s): %s", m.Subject, m.Text)
		}
		if strings.Contains(all, "support.example.com") || strings.Contains(all, "SECRET-BODY-77") {
			t.Fatalf("requester email leaks content or the agent UI (%s): %s", m.Subject, all)
		}
	}
	// Staff emails are unchanged: agent UI link and the reply text.
	watcherOrQueue := append(sentTo(f.mail, "marco@example.com"), sentTo(f.mail, "dana@example.com")...)
	if len(watcherOrQueue) == 0 || !strings.Contains(watcherOrQueue[0].Text, "https://support.example.com/tickets/1") {
		t.Fatalf("staff emails: %+v", watcherOrQueue)
	}

	// An empty value clears the link.
	put, v = f.settingsPut(map[string]any{"notifications": map[string]any{"requesterLinkBaseUrl": ""}})
	got = f.do("PUT", "/api/v1/settings", put, bearer(f.admin.Access), ifMatchV(v)).expect(200).JSON()
	if obj(got, "notifications")["requesterLinkBaseUrl"] != "" {
		t.Fatalf("not cleared: %v", got)
	}
}
