package runtime

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/webhooks/webhookstest"
)

// fieldErr returns the field names of a 422 problem.
func fieldErrs(r result) []string {
	var out []string
	for _, e := range r.Problem().Errors {
		out = append(out, e.Field)
	}
	return out
}

func hasField(r result, field string) bool {
	for _, f := range fieldErrs(r) {
		if f == field {
			return true
		}
	}
	return false
}

// TestWebhookName: the name is required on create, trimmed, validated, editable
// with If-Match and returned by get.
func TestWebhookName(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	path := "/api/v1/webhooks"
	body := func(name any) map[string]any {
		return map[string]any{"name": name, "url": hs.URL + "/hook", "eventTypes": []string{"ticket.created"}}
	}
	f.do("POST", path, map[string]any{"url": hs.URL + "/hook", "eventTypes": []string{"ticket.created"}}, bearer(f.admin.Access)).expect(422)
	f.do("POST", path, body("   "), bearer(f.admin.Access)).expect(422)
	f.do("POST", path, body(strings.Repeat("a", 81)), bearer(f.admin.Access)).expect(422)
	f.do("POST", path, body("a\x07b"), bearer(f.admin.Access)).expect(422)
	created := f.do("POST", path, body("  CRM sync "), bearer(f.admin.Access)).expect(201)
	id := created.Str("id")
	if got := created.JSON()["name"]; got != "CRM sync" {
		t.Fatalf("name = %v", got)
	}
	f.do("PATCH", path+"/"+id, map[string]any{"name": ""}, bearer(f.admin.Access), ifMatchV(1)).expect(422)
	patched := f.do("PATCH", path+"/"+id, map[string]any{"name": "CRM"}, bearer(f.admin.Access), ifMatchV(1)).expect(200).JSON()
	if patched["name"] != "CRM" {
		t.Fatalf("patched name = %v", patched["name"])
	}
	got := f.do("GET", path+"/"+id, nil, bearer(f.admin.Access)).expect(200).JSON()
	if got["name"] != "CRM" {
		t.Fatalf("get name = %v", got["name"])
	}
}

// TestWebhookHeadersAndAuth: extra headers and Basic/token auth are sent
// with deliveries (including test deliveries); reserved names are refused;
// secrets are write-only, encrypted at rest and kept when omitted on update.
func TestWebhookHeadersAndAuth(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment
	hs := newHookServer(t)
	path := "/api/v1/webhooks"
	base := func(extra map[string]any) map[string]any {
		b := map[string]any{"name": "Test hook", "url": hs.URL + "/hook", "eventTypes": []string{"ticket.created"}}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	// Reserved and invalid header names, CR/LF values, too many headers.
	for _, tc := range []struct {
		body  map[string]any
		field string
	}{
		{base(map[string]any{"headers": []map[string]any{{"name": "Content-Type", "value": "x"}}}), "headers[0].name"},
		{base(map[string]any{"headers": []map[string]any{{"name": "Host", "value": "x"}}}), "headers[0].name"},
		{base(map[string]any{"headers": []map[string]any{{"name": "x-open-kase-event-id", "value": "x"}}}), "headers[0].name"},
		{base(map[string]any{"headers": []map[string]any{{"name": "Bad Name", "value": "x"}}}), "headers[0].name"},
		{base(map[string]any{"headers": []map[string]any{{"name": "X-A", "value": "a\r\nInjected: 1"}}}), "headers[0].value"},
		{base(map[string]any{"headers": []map[string]any{{"name": "X-A"}}}), "headers[0].value"},
		{base(map[string]any{"headers": []map[string]any{{"name": "X-A", "value": "1"}, {"name": "x-a", "value": "2"}}}), "headers[1].name"},
		{base(map[string]any{"auth": map[string]any{"type": "token", "token": "t"}, "headers": []map[string]any{{"name": "Authorization", "value": "x"}}}), "headers[0].name"},
		{base(map[string]any{"auth": map[string]any{"type": "basic", "password": "p"}}), "auth.username"},
		{base(map[string]any{"auth": map[string]any{"type": "basic", "username": "a:b", "password": "p"}}), "auth.username"},
		{base(map[string]any{"auth": map[string]any{"type": "token"}}), "auth.token"},
		{base(map[string]any{"auth": map[string]any{"type": "digest"}}), "auth.type"},
	} {
		r := f.do("POST", path, tc.body, bearer(f.admin.Access)).expect(422)
		if !hasField(r, tc.field) {
			t.Fatalf("%v: want error on %s, got %s", tc.body, tc.field, r.Body)
		}
	}
	many := []map[string]any{}
	for i := range 21 {
		many = append(many, map[string]any{"name": "X-H" + string(rune('a'+i)), "value": "v"})
	}
	if r := f.do("POST", path, base(map[string]any{"headers": many}), bearer(f.admin.Access)).expect(422); !hasField(r, "headers") {
		t.Fatalf("too many headers: %s", r.Body)
	}
	// Authorization is an ordinary header while auth is none.
	f.do("POST", path, base(map[string]any{"active": false, "headers": []map[string]any{{"name": "Authorization", "value": "Custom abc"}}}),
		bearer(f.admin.Access)).expect(201)

	const headerSecret, password, token = "hdr-secret-Q1w2", "basic-pass-E3r4", "bearer-tok-T5y6"
	created := f.do("POST", path, base(map[string]any{
		"headers": []map[string]any{{"name": "X-Api-Key", "value": headerSecret}, {"name": "X-Tenant", "value": "acme"}},
		"auth":    map[string]any{"type": "basic", "username": "hooks", "password": password},
	}), bearer(f.admin.Access)).expect(201)
	for _, secret := range []string{headerSecret, password, "acme"} {
		if strings.Contains(string(created.Body), secret) {
			t.Fatalf("secret %q returned: %s", secret, created.Body)
		}
	}
	cj := created.JSON()
	auth := obj(cj, "auth")
	if auth["type"] != "basic" || auth["username"] != "hooks" || auth["passwordConfigured"] != true || auth["tokenConfigured"] != false {
		t.Fatalf("auth: %v", auth)
	}
	hdrs := cj["headers"].([]any)
	if len(hdrs) != 2 || hdrs[0].(map[string]any)["name"] != "X-Api-Key" || hdrs[0].(map[string]any)["valueConfigured"] != true {
		t.Fatalf("headers: %v", hdrs)
	}
	id := cj["id"].(string)
	raw, err := f.coll("webhookEndpoints").FindOne(context.Background(), bson.D{{Key: "_id", Value: mustObjectID(t, id)}}).Raw()
	if err != nil || strings.Contains(raw.String(), headerSecret) || strings.Contains(raw.String(), password) {
		t.Fatalf("plaintext at rest: %v %v", raw, err)
	}

	f.createTicket(f.integration, map[string]any{})
	f.drain()
	reqs := hs.all()
	if len(reqs) != 1 {
		t.Fatalf("deliveries: %d", len(reqs))
	}
	h := reqs[0].header
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("hooks:"+password))
	if h.Get("X-Api-Key") != headerSecret || h.Get("X-Tenant") != "acme" || h.Get("Authorization") != wantBasic ||
		h.Get("X-Open-Kase-Event-Type") != "ticket.created" || h.Get("X-Open-Kase-Event-Id") == "" || h.Get("X-Open-Kase-Signature") != "" {
		t.Fatalf("delivered headers: %v", h)
	}

	// Keep-if-omitted: rename nothing, drop X-Tenant, change auth username
	// only; header and password values stay.
	v := int64(cj["version"].(float64))
	upd := f.do("PATCH", path+"/"+id, map[string]any{"headers": []map[string]any{{"name": "x-api-key"}},
		"auth": map[string]any{"type": "basic", "username": "hooks2"}}, bearer(f.admin.Access), ifMatchV(v)).expect(200)
	if len(upd.JSON()["headers"].([]any)) != 1 || obj(upd.JSON(), "auth")["passwordConfigured"] != true {
		t.Fatalf("update: %s", upd.Body)
	}
	// Switching to token auth needs a token; then the token is sent.
	f.do("PATCH", path+"/"+id, map[string]any{"auth": map[string]any{"type": "token"}}, bearer(f.admin.Access), ifMatchV(upd.Version())).expect(422)
	upd = f.do("PATCH", path+"/"+id, map[string]any{"auth": map[string]any{"type": "token", "token": token}}, bearer(f.admin.Access),
		ifMatchV(upd.Version())).expect(200)
	if a := obj(upd.JSON(), "auth"); a["type"] != "token" || a["passwordConfigured"] != false || a["tokenConfigured"] != true || strings.Contains(string(upd.Body), token) {
		t.Fatalf("token auth: %s", upd.Body)
	}
	// A PATCH without auth keeps the token.
	upd = f.do("PATCH", path+"/"+id, map[string]any{"auth": map[string]any{"type": "token"}, "active": true}, bearer(f.admin.Access), ifMatchV(upd.Version())).expect(200)
	f.do("POST", path+"/"+id+"/test", nil, bearer(f.admin.Access)).expect(202)
	f.drain()
	last := hs.all()[len(hs.all())-1]
	if last.header.Get("Authorization") != "Bearer "+token || last.header.Get("X-Api-Key") != headerSecret || last.header.Get("X-Tenant") != "" ||
		last.header.Get("X-Open-Kase-Event-Type") != "webhook.test" {
		t.Fatalf("test delivery headers: %v", last.header)
	}
	if got := f.do("GET", path+"/"+id, nil, bearer(f.admin.Access)).expect(200); strings.Contains(string(got.Body), token) || strings.Contains(string(got.Body), headerSecret) {
		t.Fatalf("GET leaked a secret: %s", got.Body)
	}
	// Choosing an auth type while an Authorization header is stored fails.
	other := f.do("POST", path, base(map[string]any{"active": false, "headers": []map[string]any{{"name": "Authorization", "value": "Custom abc"}}}),
		bearer(f.admin.Access)).expect(201)
	r := f.do("PATCH", path+"/"+other.JSON()["id"].(string), map[string]any{"auth": map[string]any{"type": "token", "token": "x"}},
		bearer(f.admin.Access), ifMatchV(other.Version())).expect(422)
	if !hasField(r, "auth.type") {
		t.Fatalf("auth over stored Authorization header: %s", r.Body)
	}
	// Back to none clears the secrets.
	upd = f.do("PATCH", path+"/"+id, map[string]any{"auth": map[string]any{"type": "none"}}, bearer(f.admin.Access), ifMatchV(upd.Version())).expect(200)
	if a := obj(upd.JSON(), "auth"); a["type"] != "none" || a["tokenConfigured"] != false {
		t.Fatalf("none: %v", a)
	}
}

// tlsHookServer is an HTTPS receiver with a private CA, optionally
// requiring a client certificate.
type tlsHookServer struct {
	url  string
	mu   sync.Mutex
	hits int
}

func newTLSHookServer(t *testing.T, ca webhookstest.Pair, clientCA *webhookstest.Pair) *tlsHookServer {
	s := &tlsHookServer{}
	srv := webhookstest.Server(t, ca, clientCA, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	s.url = srv.URL + "/hook"
	return s
}

func (s *tlsHookServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// TestWebhookCustomCAAndClientCertificate: a receiver with a private CA is
// reached only with the CA configured; a receiver requiring mutual TLS only
// with the configured client certificate; mismatched or invalid PEM is a
// 422; the key is write-only and kept when omitted.
func TestWebhookCustomCAAndClientCertificate(t *testing.T) {
	f := newNotifyFixture(t)
	f.c.Boot.Env = EnvDevelopment // loopback test receivers
	path := "/api/v1/webhooks"
	ca := webhookstest.NewCA(t, "Example Private CA")
	clientCA := webhookstest.NewCA(t, "Example Client CA")
	client := clientCA.Client(t, "open-kase-webhooks")
	other := clientCA.Client(t, "someone-else")

	testDelivery := func(id string) map[string]any {
		t.Helper()
		d := f.do("POST", path+"/"+id+"/test", nil, bearer(f.admin.Access)).expect(202).JSON()
		f.drain()
		did := obj(d, "delivery")["id"].(string)
		return f.do("GET", path+"/"+id+"/deliveries/"+did, nil, bearer(f.admin.Access)).expect(200).JSON()
	}

	// Private CA: fails without the CA, succeeds with it.
	plain := newTLSHookServer(t, ca, nil)
	hook := f.do("POST", path, map[string]any{"name": "Test hook", "url": plain.url, "eventTypes": []string{"ticket.created"}}, bearer(f.admin.Access)).expect(201)
	id := hook.JSON()["id"].(string)
	if d := testDelivery(id); d["status"] == "sent" || d["errorClass"] != "tls" || plain.count() != 0 {
		t.Fatalf("delivery without the CA: %v", d)
	}
	f.do("PATCH", path+"/"+id, map[string]any{"tls": map[string]any{"caCertificate": "garbage"}}, bearer(f.admin.Access), ifMatchV(hook.Version())).
		expect(422)
	upd := f.do("PATCH", path+"/"+id, map[string]any{"tls": map[string]any{"caCertificate": ca.CertPEM}}, bearer(f.admin.Access), ifMatchV(hook.Version())).expect(200)
	tlsOut := obj(upd.JSON(), "tls")
	cas, _ := tlsOut["caCertificates"].([]any)
	if len(cas) != 1 || !strings.Contains(cas[0].(map[string]any)["subject"].(string), "Example Private CA") || cas[0].(map[string]any)["notAfter"] == nil {
		t.Fatalf("CA summary: %v", tlsOut)
	}
	if d := testDelivery(id); d["status"] != "sent" || plain.count() != 1 {
		t.Fatalf("delivery with the CA: %v", d)
	}

	// Mutual TLS.
	mtls := newTLSHookServer(t, ca, &clientCA)
	// Mismatched certificate and key.
	r := f.do("POST", path, map[string]any{"name": "Test hook", "url": mtls.url, "eventTypes": []string{"ticket.created"},
		"tls": map[string]any{"caCertificate": ca.CertPEM, "clientCertificate": client.CertPEM, "clientKey": other.KeyPEM}}, bearer(f.admin.Access)).expect(422)
	if !hasField(r, "tls.clientKey") {
		t.Fatalf("mismatch: %s", r.Body)
	}
	// Key without certificate; certificate without key; bad certificate.
	for _, tc := range []struct {
		tls   map[string]any
		field string
	}{
		{map[string]any{"clientKey": client.KeyPEM}, "tls.clientKey"},
		{map[string]any{"clientCertificate": client.CertPEM}, "tls.clientKey"},
		{map[string]any{"clientCertificate": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----", "clientKey": client.KeyPEM}, "tls.clientCertificate"},
	} {
		r := f.do("POST", path, map[string]any{"name": "Test hook", "url": mtls.url, "eventTypes": []string{"ticket.created"}, "tls": tc.tls}, bearer(f.admin.Access)).expect(422)
		if !hasField(r, tc.field) {
			t.Fatalf("%v: want %s, got %s", tc.tls, tc.field, r.Body)
		}
	}
	// Without a client certificate the receiver refuses the handshake.
	m := f.do("POST", path, map[string]any{"name": "Test hook", "url": mtls.url, "eventTypes": []string{"ticket.created"},
		"tls": map[string]any{"caCertificate": ca.CertPEM}}, bearer(f.admin.Access)).expect(201)
	mid := m.JSON()["id"].(string)
	if d := testDelivery(mid); d["status"] == "sent" || mtls.count() != 0 {
		t.Fatalf("mTLS without a client certificate: %v", d)
	}
	upd = f.do("PATCH", path+"/"+mid, map[string]any{"tls": map[string]any{"clientCertificate": client.CertPEM, "clientKey": client.KeyPEM}},
		bearer(f.admin.Access), ifMatchV(m.Version())).expect(200)
	if strings.Contains(string(upd.Body), "PRIVATE KEY") {
		t.Fatalf("client key returned: %s", upd.Body)
	}
	tlsOut = obj(upd.JSON(), "tls")
	info := obj(tlsOut, "clientCertificateInfo")
	if tlsOut["clientKeyConfigured"] != true || !strings.Contains(info["subject"].(string), "open-kase-webhooks") ||
		!strings.Contains(info["issuer"].(string), "Example Client CA") || obj(upd.JSON(), "tls")["caCertificate"] != strings.TrimSpace(ca.CertPEM) {
		t.Fatalf("client certificate summary: %v", tlsOut)
	}
	raw, err := f.coll("webhookEndpoints").FindOne(context.Background(), bson.D{{Key: "_id", Value: mustObjectID(t, mid)}}).Raw()
	if err != nil || strings.Contains(raw.String(), "PRIVATE KEY") {
		t.Fatalf("client key at rest in plaintext: %v", err)
	}
	if d := testDelivery(mid); d["status"] != "sent" || mtls.count() != 1 {
		t.Fatalf("mTLS with the client certificate: %v", d)
	}
	// Replacing the certificate keeps the stored key only if it matches.
	r = f.do("PATCH", path+"/"+mid, map[string]any{"tls": map[string]any{"clientCertificate": other.CertPEM}}, bearer(f.admin.Access), ifMatchV(upd.Version())).expect(422)
	if !hasField(r, "tls.clientKey") {
		t.Fatalf("new certificate with the old key: %s", r.Body)
	}
	// An unrelated PATCH keeps the key.
	upd = f.do("PATCH", path+"/"+mid, map[string]any{"active": true, "tls": map[string]any{"clientKey": ""}}, bearer(f.admin.Access), ifMatchV(upd.Version())).expect(200)
	if obj(upd.JSON(), "tls")["clientKeyConfigured"] != true {
		t.Fatalf("key dropped: %s", upd.Body)
	}
	// Removing the client certificate removes the key; delivery fails again.
	upd = f.do("PATCH", path+"/"+mid, map[string]any{"tls": map[string]any{"clientCertificate": ""}}, bearer(f.admin.Access), ifMatchV(upd.Version())).expect(200)
	if tl := obj(upd.JSON(), "tls"); tl["clientKeyConfigured"] != false || tl["clientCertificateInfo"] != nil {
		t.Fatalf("removal: %v", tl)
	}
	if d := testDelivery(mid); d["status"] == "sent" || mtls.count() != 1 {
		t.Fatalf("mTLS after removal: %v", d)
	}
}

func mustObjectID(t *testing.T, hex string) bson.ObjectID {
	t.Helper()
	id, err := bson.ObjectIDFromHex(hex)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
