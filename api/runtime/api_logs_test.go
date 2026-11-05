package runtime

import (
	"bytes"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/settings"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLogsCarryNoSecretsOrBodies drives sign-in, session, ticket, SMTP,
// and webhook flows — including their failure paths, which is where
// errors get logged — with every logger at debug level, then checks that no
// password, token, credential, secret or message body reached the logs.
func TestLogsCarryNoSecretsOrBodies(t *testing.T) {
	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := newTicketFixtureOn(newHarnessWithLog(t, log))
	f.c.Boot.Env = EnvDevelopment // plain http to the local receivers
	f.c.JobBackoff = func(int) time.Duration { return time.Millisecond }
	f.c.WebhookJitter = func(time.Duration) time.Duration { return 0 }
	f.c.Mailers = notifications.SMTPMailer // the real go-mail adapter
	smtpSrv := newFakeSMTP(t)
	port := int64(smtpSrv.ln.Addr().(*net.TCPAddr).Port)
	f.updateSettings(func(s *settings.Settings) {
		s.Product.BaseURL = "https://support.example.com"
		s.SMTP = settings.SMTPSettings{Enabled: true, Host: "127.0.0.1", Port: port, FromAddress: "support@example.com",
			FromName: "Support", TLSMode: "none"}
	})

	var secrets []string
	keep := func(s string) string {
		if s == "" {
			t.Fatal("empty secret captured")
		}
		secrets = append(secrets, s)
		return s
	}
	keep(adminPassword)
	keep(agentPassword)
	keep(f.dana.Access)
	keep(f.dana.Refresh)
	keep(f.dana.CSRF)
	keep(f.admin.Access)
	keep(f.admin.Refresh)
	keep(f.integration)

	// Sign-in failures, refresh rotation, logout.
	f.do("POST", "/api/v1/auth/login", map[string]string{"login": adminEmail, "password": keep("wrong-password-Zq81")}, header("Origin", testOrigin)).expect(401)
	f.do("POST", "/api/v1/auth/login", map[string]string{"login": "ghost@example.com", "password": keep("ghost-password-Lm22")}, header("Origin", testOrigin)).expect(401)
	r := f.refresh(f.dana).expect(200)
	keep(r.Str("accessToken"))
	keep(r.Cookies["kase_refresh"].Value)
	// Use the successor once: from then on the rotated token is a replay even
	// inside the grace window that lets two tabs refresh together (OC-AUTH-01).
	next := f.dana
	next.Refresh = r.Cookies["kase_refresh"].Value
	r2 := f.refresh(next).expect(200)
	keep(r2.Str("accessToken"))
	keep(r2.Cookies["kase_refresh"].Value)
	f.refresh(f.dana).expect(401)                       // reuse of the rotated token
	f.dana = f.login("dana@example.com", agentPassword) // reuse revoked the old session
	keep(f.dana.Access)
	f.do("GET", "/api/v1/me", nil, bearer("eyJhbGciOiJFZERTQSJ9.forged-token-Xy77.sig")).expect(401)
	keep("forged-token-Xy77")

	// Tickets, messages and search terms.
	publicBody := keep("public-body-marker-Hq3w")
	noteBody := keep("agent-note-marker-Pv9k")
	f.createTicket(f.integration, map[string]any{"body": "Customer says " + publicBody, "subject": "Login problem"})
	f.reply(f.dana.Access, "1", "agent", "Note: "+noteBody)
	f.reply(f.dana.Access, "1", "public", "Reply "+publicBody)
	f.do("GET", "/api/v1/search?q="+keep("search-term-Rr5t"), nil, bearer(f.dana.Access)).expect(200)
	f.do("GET", "/api/v1/tickets?q="+keep("subject-term-Kk2u"), nil, bearer(f.dana.Access)).expect(200)

	// Password reset: the token travels by email only.
	f.do("POST", "/api/v1/auth/password-reset/request", map[string]any{"email": "dana@example.com"}, header("Origin", testOrigin)).expect(202)
	f.drain()
	var resetToken string
	for _, m := range smtpSrv.received() {
		m = strings.NewReplacer("=\r\n", "", "=3D", "=").Replace(m) // quoted-printable
		if mm := regexp.MustCompile(`token=([A-Za-z0-9_-]{43})`).FindStringSubmatch(m); mm != nil {
			resetToken = mm[1]
		}
	}
	keep(resetToken)
	newPassword := keep("a brand new password Wx4")
	f.do("POST", "/api/v1/auth/password-reset/complete", map[string]any{"token": resetToken, "password": newPassword}, header("Origin", testOrigin)).expect(204)
	f.dana = f.login("dana@example.com", newPassword) // the reset ended every session
	keep(f.dana.Access)
	f.do("POST", "/api/v1/me/password", map[string]any{"currentPassword": agentPassword, "newPassword": keep("another new password Jq7")},
		bearer(f.marco.Access)).expect(204)

	// SMTP with credentials the server cannot accept: every send fails
	// and is logged by the job worker.
	smtpPassword := keep("smtp-password-Bn6y")
	cur := f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200).JSON()
	f.do("PUT", "/api/v1/settings/smtp", map[string]any{"enabled": true, "host": "127.0.0.1", "port": port, "username": "mailer",
		"password": smtpPassword, "fromAddress": "support@example.com", "fromName": "Support", "tlsMode": "starttls"},
		bearer(f.admin.Access), ifMatchV(int64(cur["configVersion"].(float64)))).expect(200)
	f.do("POST", "/api/v1/settings/smtp/test", map[string]any{"to": "ops@example.com"}, bearer(f.admin.Access)).expect(202)
	f.reply(f.dana.Access, "1", "public", "Second reply "+publicBody)

	// Webhooks: the receiver fails, so deliveries are retried and logged.
	hs := newHookServer(t)
	hs.respond = func(int, []byte) int { return 500 }
	wh := f.do("POST", "/api/v1/webhooks", map[string]any{"name": "Test hook", "url": hs.URL + "/hook?token=" + keep("url-query-secret-Tt0p"),
		"eventTypes": []string{"ticket.created", "ticket.message.public"},
		"headers":    []map[string]any{{"name": "X-Api-Key", "value": keep("webhook-header-secret-Hd4q")}},
		"auth":       map[string]any{"type": "token", "token": keep("webhook-bearer-secret-Bq8z")}}, bearer(f.admin.Access)).expect(201).JSON()
	f.do("POST", "/api/v1/webhooks/"+wh["id"].(string)+"/test", nil, bearer(f.admin.Access))
	f.reply(f.dana.Access, "1", "public", "Third reply "+publicBody)
	f.drain()
	if len(hs.all()) == 0 {
		t.Fatal("webhook receiver got nothing")
	}

	out := logs.String()
	// The capture works and saw the failure paths.
	for _, want := range []string{`"msg":"http request"`, `"route":"/api/v1/auth/login"`, `"status":401`} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs lack %s; capture not working", want)
		}
	}
	if !strings.Contains(out, `"level":"WARN"`) && !strings.Contains(out, `"level":"ERROR"`) {
		t.Fatal("no warning or error was logged; the failure paths were not exercised")
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			i := strings.Index(out, s)
			lo, hi := max(0, i-300), min(len(out), i+len(s)+100)
			t.Errorf("secret %q found in logs: …%s…", s, out[lo:hi])
		}
	}
	// Bearer/Basic credentials or cookies never appear in any form.
	for _, re := range []string{`(?i)authorization`, `kase_refresh=`, `(?i)"password":`, `(?i)"api_key":"[^"]`} {
		if m := regexp.MustCompile(re).FindString(out); m != "" {
			t.Errorf("logs contain %q", m)
		}
	}
	t.Logf("checked %d secrets against %d bytes of logs", len(secrets), len(out))
}
