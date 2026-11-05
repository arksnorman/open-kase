package runtime

import (
	"strings"
	"testing"
	"time"
)

// TestAuditLogListing covers GET /audit-events (admin Logs area): admin
// only, filters, cursor pagination, names, and that no message body,
// secret or token material is returned.
func TestAuditLogListing(t *testing.T) {
	f := newTicketFixture(t)
	tk := f.createTicket(f.admin.Access, map[string]any{})
	key, id := tk.Ref(), tk.Str("id")
	f.reply(f.dana.Access, key, "agent", "NOTE-BODY-7f3a customer runs the legacy cluster")
	botID, bot := f.token("Support bot", "tickets:all")
	f.do("POST", tpath(key, "/messages"), map[string]any{"visibility": "agent", "body": "BOT-NOTE-91c2 checked MX"}, bearer(bot)).expect(201)
	// A secret written through the settings API.
	snap := f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200).JSON()
	f.do("PUT", "/api/v1/settings/smtp", map[string]any{"enabled": false, "host": "smtp.example.com", "port": 587, "username": "mailer",
		"password": "SMTP-SECRET-4411", "fromAddress": "support@example.com", "fromName": "Support", "tlsMode": "starttls"},
		bearer(f.admin.Access), ifMatchV(int64(snap["configVersion"].(float64)))).expect(200)

	all := f.do("GET", "/api/v1/audit-events?limit=100", nil, bearer(f.admin.Access)).expect(200)
	for _, secret := range []string{"NOTE-BODY-7f3a", "BOT-NOTE-91c2", "SMTP-SECRET-4411", bot, f.admin.Access} {
		if strings.Contains(string(all.Body), secret) {
			t.Fatalf("audit log exposes %q: %s", secret, all.Body)
		}
	}
	items := all.Items()
	if len(items) < 5 {
		t.Fatalf("too few events: %s", all.Body)
	}
	var sawNote, sawSettings bool
	for i, it := range items {
		at, _ := time.Parse(time.RFC3339Nano, it["createdAt"].(string))
		prev, _ := time.Parse(time.RFC3339Nano, items[max(i-1, 0)]["createdAt"].(string))
		if at.IsZero() || at.After(prev) {
			t.Fatalf("not newest first: %s", all.Body)
		}
		actor, _ := it["actor"].(map[string]any)
		target, _ := it["target"].(map[string]any)
		switch it["action"] {
		case "message.created":
			tref, _ := target["ticket"].(map[string]any)
			if target["type"] != "ticket" || refOf(tref) != key || tref["subject"] != "Cannot receive external email" {
				t.Fatalf("message target: %v", it)
			}
			if actor["type"] == "user" && actor["name"] == "Dana Okafor" {
				sawNote = true
			}
		case "settings.updated":
			sawSettings = true
		}
	}
	if !sawNote || !sawSettings {
		t.Fatalf("missing events (note %v, settings %v): %s", sawNote, sawSettings, all.Body)
	}

	// Actor type token: the bot, with its name and scopes.
	toks := f.do("GET", "/api/v1/audit-events?actorType=token", nil, bearer(f.admin.Access)).expect(200).Items()
	if len(toks) == 0 {
		t.Fatal("no token events")
	}
	for _, it := range toks {
		a := it["actor"].(map[string]any)
		if a["type"] != "token" || a["id"] != botID || a["name"] != "Support bot" || len(a["scopes"].([]any)) == 0 {
			t.Fatalf("token actor: %v", it)
		}
	}
	// Actor ID and action prefix.
	for _, it := range f.do("GET", "/api/v1/audit-events?actorId="+f.dana.UserId+"&action=message.", nil, bearer(f.admin.Access)).expect(200).Items() {
		if it["action"] != "message.created" || it["actor"].(map[string]any)["id"] != f.dana.UserId {
			t.Fatalf("actor/action filter: %v", it)
		}
	}
	// System actors carry their catalog name ("System · Installation", …).
	for _, it := range f.do("GET", "/api/v1/audit-events?actorType=system", nil, bearer(f.admin.Access)).expect(200).Items() {
		if a := it["actor"].(map[string]any); a["type"] != "system" || !strings.HasPrefix(a["name"].(string), "System · ") {
			t.Fatalf("system actor: %v", it)
		}
	}
	// Ticket by key, number and ID give the same events, all of that ticket.
	byKey := f.do("GET", "/api/v1/audit-events?ticket="+key, nil, bearer(f.admin.Access)).expect(200).Items()
	num := key[strings.LastIndexByte(key, '-')+1:]
	byNum := f.do("GET", "/api/v1/audit-events?ticket="+num, nil, bearer(f.admin.Access)).expect(200).Items()
	byID := f.do("GET", "/api/v1/audit-events?ticket="+id, nil, bearer(f.admin.Access)).expect(200).Items()
	if len(byKey) < 3 || len(byKey) != len(byNum) || len(byKey) != len(byID) {
		t.Fatalf("ticket filter: key %d, number %d, id %d", len(byKey), len(byNum), len(byID))
	}
	for _, it := range byKey {
		if it["target"].(map[string]any)["id"] != id {
			t.Fatalf("other ticket's event: %v", it)
		}
	}
	if n := len(f.do("GET", "/api/v1/audit-events?ticket=99999", nil, bearer(f.admin.Access)).expect(200).Items()); n != 0 {
		t.Fatalf("unknown ticket number: %d events", n)
	}
	// Dates (UTC days, inclusive).
	today := time.Now().UTC().Format(time.DateOnly)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format(time.DateOnly)
	if n := len(f.do("GET", "/api/v1/audit-events?from="+tomorrow, nil, bearer(f.admin.Access)).expect(200).Items()); n != 0 {
		t.Fatalf("from tomorrow: %d events", n)
	}
	if n := len(f.do("GET", "/api/v1/audit-events?from="+today+"&to="+today, nil, bearer(f.admin.Access)).expect(200).Items()); n == 0 {
		t.Fatal("today: no events")
	}
	f.do("GET", "/api/v1/audit-events?from="+tomorrow+"&to="+today, nil, bearer(f.admin.Access)).expect(422)
	f.do("GET", "/api/v1/audit-events?actorType=robot", nil, bearer(f.admin.Access)).expect(422)
	f.do("GET", "/api/v1/audit-events?action=%24where", nil, bearer(f.admin.Access)).expect(422)

	// Pagination: pages of 2 walk the same events without overlap.
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		path := "/api/v1/audit-events?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := f.do("GET", path, nil, bearer(f.admin.Access)).expect(200)
		for _, it := range r.Items() {
			if seen[it["id"].(string)] {
				t.Fatalf("event %v on two pages", it["id"])
			}
			seen[it["id"].(string)] = true
		}
		pages++
		next, _ := r.JSON()["nextCursor"].(string)
		if next == "" || pages > 100 {
			break
		}
		cursor = next
	}
	if len(seen) != len(items) || pages < 2 {
		t.Fatalf("paged %d events in %d pages, listed %d", len(seen), pages, len(items))
	}

	// Admins only: agents and tokens (any scope) are refused.
	f.do("GET", "/api/v1/audit-events", nil, bearer(f.dana.Access)).expect(403)
	f.do("GET", "/api/v1/audit-events", nil, bearer(bot)).expect(403)
	f.do("GET", "/api/v1/audit-events", nil, bearer(f.integration)).expect(403)
	f.do("GET", "/api/v1/audit-events", nil).expect(401)
}

// TestDeliveriesShowRecipientNames checks that delivery listings name the
// user and contact recipients (and keep their IDs).
func TestDeliveriesShowRecipientNames(t *testing.T) {
	f := newNotifyFixture(t)
	f.createTicket(f.integration, map[string]any{})
	f.drain()
	items := f.do("GET", "/api/v1/deliveries", nil, bearer(f.admin.Access)).expect(200).Items()
	want := map[string]string{"Priya Raman": "priya@acme.example", "Dana Okafor": "dana@example.com", "Marco Silva": "marco@example.com"}
	got := map[string]string{}
	for _, it := range items {
		r := it["recipient"].(map[string]any)
		if r["kind"] == "user" || r["kind"] == "contact" {
			if id, _ := r["id"].(string); len(id) != 24 {
				t.Fatalf("recipient id missing: %v", r)
			}
			name, _ := r["name"].(string)
			email, _ := r["email"].(string)
			got[name] = email
		}
	}
	for name, email := range want {
		if got[name] != email {
			t.Fatalf("recipient %s: got %v (items %v)", name, got, items)
		}
	}
	// The single-delivery read carries the names too.
	one := f.do("GET", "/api/v1/deliveries/"+items[0]["id"].(string), nil, bearer(f.admin.Access)).expect(200).JSON()
	if r := one["recipient"].(map[string]any); r["name"] == nil {
		t.Fatalf("delivery read without name: %v", one)
	}
}
