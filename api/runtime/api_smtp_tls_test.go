package runtime

import (
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSMTPCustomCAAndSkipVerifyRoundTrip covers the SMTP caCertificate and
// tlsSkipVerify fields: PUT then GET, validation and password re-entry when verification is weakened.
func TestSMTPCustomCAAndSkipVerifyRoundTrip(t *testing.T) {
	f := newTicketFixture(t)
	hs := httptest.NewTLSServer(nil)
	defer hs.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: hs.Certificate().Raw}))
	body := func(extra map[string]any) map[string]any {
		m := map[string]any{"enabled": false, "host": "smtp.example.com", "port": 587, "username": "mailer",
			"fromAddress": "support@example.com", "fromName": "Support", "tlsMode": "starttls"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	version := func() int64 {
		return int64(f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200).JSON()["configVersion"].(float64))
	}
	f.do("PUT", "/api/v1/settings/smtp", body(map[string]any{"password": "pw", "caCertificate": "garbage"}),
		bearer(f.admin.Access), ifMatchV(version())).expect(422)
	f.do("PUT", "/api/v1/settings/smtp", body(map[string]any{"password": "pw", "caCertificate": ca, "tlsSkipVerify": true}),
		bearer(f.admin.Access), ifMatchV(version())).expect(200)
	got := f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200).JSON()
	if got["caCertificate"] != strings.TrimSpace(ca) || got["tlsSkipVerify"] != true {
		t.Fatalf("round trip: %v", got)
	}
	// Removing the CA and skip-verify strengthens verification, so the password need not be re-sent.
	f.do("PUT", "/api/v1/settings/smtp", body(nil), bearer(f.admin.Access), ifMatchV(version())).expect(200)
	got = f.do("GET", "/api/v1/settings/smtp", nil, bearer(f.admin.Access)).expect(200).JSON()
	if got["caCertificate"] != "" || got["tlsSkipVerify"] != false || got["passwordConfigured"] != true {
		t.Fatalf("after clearing: %v", got)
	}
	// Weakening verification needs the password again.
	f.do("PUT", "/api/v1/settings/smtp", body(map[string]any{"tlsSkipVerify": true}), bearer(f.admin.Access), ifMatchV(version())).expect(422)
	f.do("PUT", "/api/v1/settings/smtp", body(map[string]any{"caCertificate": ca}), bearer(f.admin.Access), ifMatchV(version())).expect(422)
	f.do("PUT", "/api/v1/settings/smtp", body(map[string]any{"tlsSkipVerify": true, "password": "pw"}), bearer(f.admin.Access), ifMatchV(version())).expect(200)
}
