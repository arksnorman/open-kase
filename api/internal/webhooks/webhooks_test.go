package webhooks

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/webhooks/webhookstest"
)

var t0 = time.Unix(1767225600, 0) // 2026-01-01T00:00:00Z

func TestEnvelopeMarshal(t *testing.T) {
	env := Envelope{
		EventId:    "evt_1",
		Type:       "ticket.statusChanged",
		OccurredAt: time.Date(2026, 10, 2, 9, 0, 0, 123456789, time.FixedZone("x", 3600)),
		Ticket:     &TicketRef{Id: "t1", Number: 42, Revision: 7},
		Data:       json.RawMessage(`{"status":"resolved"}`),
	}
	b, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"eventId":"evt_1","type":"ticket.statusChanged","occurredAt":"2026-10-02T08:00:00.123Z","ticket":{"id":"t1","number":42,"revision":7},"data":{"status":"resolved"}}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	for name, e := range map[string]Envelope{
		"no event id":  {Type: "x", OccurredAt: t0},
		"no time":      {EventId: "e", Type: "x"},
		"bad data":     {EventId: "e", Type: "x", OccurredAt: t0, Data: json.RawMessage(`{`)},
		"ticket no id": {EventId: "e", Type: "x", OccurredAt: t0, Ticket: &TicketRef{}},
	} {
		if _, err := e.Marshal(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCheckIP(t *testing.T) {
	var p Policy
	for _, s := range []string{
		"127.0.0.1", "::1", "169.254.169.254", "169.254.1.1",
		"0.0.0.0", "::", "224.0.0.1", "255.255.255.255", "fe80::1", "fd00:ec2::254",
		"::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::", "198.18.0.1", "240.0.0.1",
		"100.100.100.200", "ff02::1",
	} {
		if err := p.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrDestinationBlocked) {
			t.Errorf("%s allowed", s)
		}
	}
	// Public and private networks (RFC 1918, unique local, CGNAT) are allowed.
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8",
		"10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1", "fc00::1", "::ffff:10.0.0.1"} {
		if err := p.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s blocked: %v", s, err)
		}
	}
	// Development allows loopback receivers, nothing else extra.
	dev := Policy{AllowLoopback: true}
	if err := dev.CheckIP(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Errorf("dev loopback blocked: %v", err)
	}
	for _, s := range []string{"169.254.169.254", "169.254.1.1", "0.0.0.0"} {
		if err := dev.CheckIP(netip.MustParseAddr(s)); err == nil {
			t.Errorf("dev allowed %s", s)
		}
	}
}

func TestCheckURL(t *testing.T) {
	var p Policy
	for _, u := range []string{
		"http://example.com/hook", "ftp://example.com/", "https://user:pw@example.com/", "https:///path",
		"https://127.0.0.1/", "https://[::1]/", "https://localhost/", "https://api.localhost./",
		"https://169.254.169.254/latest/meta-data", "https://example.com:99999/", "mailto:a@example.com",
		"https://example.com/" + strings.Repeat("a", MaxURLLength),
	} {
		if _, err := p.CheckURL(u); !errors.Is(err, ErrDestinationBlocked) {
			t.Errorf("%s accepted", u)
		}
	}
	u, err := p.CheckURL("https://hooks.example.com:8443/in?x=1#frag")
	if err != nil || u.Fragment != "" || u.Host != "hooks.example.com:8443" {
		t.Fatalf("valid URL: %v %v", u, err)
	}
	dev := Policy{AllowHTTP: true}
	if _, err := dev.CheckURL("http://example.com/hook"); err != nil {
		t.Fatalf("dev http: %v", err)
	}
}

type receiver struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
}

func (rc *receiver) record(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.headers = append(rc.headers, r.Header.Clone())
	rc.bodies = append(rc.bodies, b)
	rc.mu.Unlock()
	return b
}

func tlsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, Policy) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return srv, Policy{
		AllowLoopback: true,
		RootCAs:       roots,
		Timeout:       2 * time.Second,
	}
}

func TestDeliverRequestHeaders(t *testing.T) {
	var rc receiver
	srv, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		rc.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	body := []byte(`{"eventId":"evt_1"}`)
	res := NewClient(p).Deliver(context.Background(), Request{URL: srv.URL + "/hook", EventId: "evt_1", EventType: "ticket.created", Body: body,
		Headers: []Header{{Name: "X-Tenant", Value: "acme"}, {Name: "Content-Type", Value: "text/plain"}}, Authorization: "Bearer tok-1"})
	if res.Outcome != OutcomeDelivered || res.StatusCode != http.StatusNoContent || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	h := rc.headers[0]
	if h.Get(HeaderEventID) != "evt_1" || h.Get(HeaderEventType) != "ticket.created" || h.Get("Content-Type") != "application/json" ||
		h.Get("X-Tenant") != "acme" || h.Get("Authorization") != "Bearer tok-1" || !strings.HasPrefix(h.Get("User-Agent"), "open-kase-webhooks/") {
		t.Fatalf("headers = %v", h)
	}
	for name := range h {
		if strings.Contains(strings.ToLower(name), "signature") || strings.Contains(strings.ToLower(name), "timestamp") {
			t.Fatalf("signature header sent: %s", name)
		}
	}
	if string(rc.bodies[0]) != string(body) {
		t.Fatalf("body changed: %s", rc.bodies[0])
	}
}

func TestDeliverBlocksLoopbackOutsideDevelopment(t *testing.T) {
	var rc receiver
	srv, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) { rc.record(r) })
	p.AllowLoopback = false
	res := NewClient(p).Deliver(context.Background(), Request{URL: srv.URL, EventId: "e", Body: []byte(`{}`)})
	if res.Outcome != OutcomePermanent || res.ErrorClass != ErrorBlocked {
		t.Fatalf("result = %+v", res)
	}
	if len(rc.bodies) != 0 {
		t.Fatal("request reached a blocked destination")
	}
}

func TestDialTimeCheckBlocksResolvedLoopback(t *testing.T) {
	// Calling the HTTP client directly skips the static CheckURL; the dialer
	// must still reject the address that "localhost" resolves to.
	var rc receiver
	srv, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) { rc.record(r) })
	p.AllowLoopback = false
	c := NewClient(p)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	req, _ := http.NewRequest(http.MethodPost, "https://localhost:"+port+"/", strings.NewReader("{}"))
	resp, err := c.http.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("dial to resolved loopback succeeded")
	}
	if !errors.Is(err, ErrDestinationBlocked) || classifyTransportError(err).ErrorClass != ErrorBlocked {
		t.Fatalf("err = %v", err)
	}
	if len(rc.bodies) != 0 {
		t.Fatal("request reached a blocked destination")
	}
}

func TestDeliverRequiresHTTPSUnlessDevelopment(t *testing.T) {
	var rc receiver
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rc.record(r) }))
	t.Cleanup(srv.Close)
	req := Request{URL: srv.URL, EventId: "e", Body: []byte(`{}`)}
	if res := NewClient(Policy{AllowLoopback: true}).Deliver(context.Background(), req); res.ErrorClass != ErrorBlocked {
		t.Fatalf("http accepted: %+v", res)
	}
	if res := NewClient(Policy{AllowLoopback: true, AllowHTTP: true}).Deliver(context.Background(), req); res.Outcome != OutcomeDelivered {
		t.Fatalf("dev http: %+v", res)
	}
}

func TestDeliverDoesNotFollowRedirects(t *testing.T) {
	var hits receiver
	target, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) { hits.record(r) })
	srv, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	res := NewClient(p).Deliver(context.Background(), Request{URL: srv.URL, EventId: "e", Body: []byte(`{}`)})
	if res.Outcome != OutcomeRetry || res.ErrorClass != ErrorRedirect || res.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("result = %+v", res)
	}
	if len(hits.bodies) != 0 {
		t.Fatal("redirect followed")
	}
}

func TestDeliverStatusClassification(t *testing.T) {
	cases := []struct {
		status     int
		retryAfter string
		outcome    Outcome
		class      ErrorClass
		wantAfter  time.Duration
	}{
		{http.StatusOK, "", OutcomeDelivered, ErrorNone, 0},
		{http.StatusServiceUnavailable, "120", OutcomeRetry, ErrorHTTPStatus, 2 * time.Minute},
		{http.StatusTooManyRequests, "999999999", OutcomeRetry, ErrorRateLimited, time.Hour},
		{http.StatusInternalServerError, "", OutcomeRetry, ErrorHTTPStatus, 0},
		{http.StatusBadRequest, "", OutcomeRetry, ErrorHTTPStatus, 0},
		{http.StatusGone, "", OutcomePermanent, ErrorGone, 0},
	}
	for _, tc := range cases {
		srv, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
			if tc.retryAfter != "" {
				w.Header().Set("Retry-After", tc.retryAfter)
			}
			w.WriteHeader(tc.status)
		})
		p.MaxRetryAfter = time.Hour
		res := NewClient(p).Deliver(context.Background(), Request{URL: srv.URL, EventId: "e", Body: []byte(`{}`)})
		if res.Outcome != tc.outcome || res.ErrorClass != tc.class || res.RetryAfter != tc.wantAfter || res.StatusCode != tc.status {
			t.Errorf("status %d: %+v", tc.status, res)
		}
	}
}

func TestDeliverTimeoutAndResponseLimit(t *testing.T) {
	slow, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(600 * time.Millisecond):
		case <-r.Context().Done():
		}
	})
	p.Timeout = 200 * time.Millisecond
	res := NewClient(p).Deliver(context.Background(), Request{URL: slow.URL, EventId: "e", Body: []byte(`{}`)})
	if res.Outcome != OutcomeRetry || res.ErrorClass != ErrorTimeout {
		t.Fatalf("timeout result = %+v", res)
	}

	big, p2 := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4<<20)))
	})
	p2.MaxResponseBytes = 1024
	res = NewClient(p2).Deliver(context.Background(), Request{URL: big.URL, EventId: "e", Body: []byte(`{}`)})
	if res.Outcome != OutcomeDelivered || len(res.ResponseSnippet) != 256 {
		t.Fatalf("large response result: outcome=%s snippet=%d", res.Outcome, len(res.ResponseSnippet))
	}
}

func TestDeliverVerifiesCertificates(t *testing.T) {
	var rc receiver
	srv, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) { rc.record(r) })
	p.RootCAs = x509.NewCertPool()
	res := NewClient(p).Deliver(context.Background(), Request{URL: srv.URL, EventId: "e", Body: []byte(`{}`)})
	if res.Outcome != OutcomeRetry || res.ErrorClass != ErrorTLS || len(rc.bodies) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestDeliverRejectsMissingEventID(t *testing.T) {
	_, p := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {})
	c := NewClient(p)
	if res := c.Deliver(context.Background(), Request{URL: "https://127.0.0.1/", Body: []byte(`{}`)}); res.ErrorClass != ErrorConfig {
		t.Fatalf("no event id: %+v", res)
	}
}

// A receiver with a certificate from a private CA is reachable only with
// that CA configured for the endpoint (added to the roots).
func TestDeliverCustomCA(t *testing.T) {
	ca := webhookstest.NewCA(t, "Private Test CA")
	var rc receiver
	srv := webhookstest.Server(t, ca, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rc.record(r) }))
	c := NewClient(Policy{AllowLoopback: true, Timeout: 2 * time.Second})
	req := Request{URL: srv.URL, EventId: "e", Body: []byte(`{}`)}
	if res := c.Deliver(context.Background(), req); res.Outcome != OutcomeRetry || res.ErrorClass != ErrorTLS {
		t.Fatalf("without the CA: %+v", res)
	}
	other := webhookstest.NewCA(t, "Other CA")
	req.TLS = &TLSConfig{CAPEM: []byte(other.CertPEM)}
	if res := c.Deliver(context.Background(), req); res.ErrorClass != ErrorTLS {
		t.Fatalf("with an unrelated CA: %+v", res)
	}
	req.TLS = &TLSConfig{CAPEM: []byte(ca.CertPEM)}
	if res := c.Deliver(context.Background(), req); res.Outcome != OutcomeDelivered {
		t.Fatalf("with the CA: %+v", res)
	}
	req.TLS = &TLSConfig{CAPEM: []byte("not pem")}
	if res := c.Deliver(context.Background(), req); res.ErrorClass != ErrorConfig {
		t.Fatalf("unparseable CA: %+v", res)
	}
	if len(rc.bodies) != 1 {
		t.Fatalf("receiver got %d requests", len(rc.bodies))
	}
}

// A receiver requiring a client certificate accepts the configured one.
func TestDeliverClientCertificate(t *testing.T) {
	ca := webhookstest.NewCA(t, "Server CA")
	clientCA := webhookstest.NewCA(t, "Client CA")
	var rc receiver
	srv := webhookstest.Server(t, ca, &clientCA, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.record(r)
		if len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != "open-kase" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	c := NewClient(Policy{AllowLoopback: true, Timeout: 2 * time.Second})
	req := Request{URL: srv.URL, EventId: "e", Body: []byte(`{}`), TLS: &TLSConfig{CAPEM: []byte(ca.CertPEM)}}
	if res := c.Deliver(context.Background(), req); res.Outcome == OutcomeDelivered || len(rc.bodies) != 0 {
		t.Fatalf("without a client certificate: %+v", res)
	}
	stranger := webhookstest.NewCA(t, "Stranger CA").Client(t, "open-kase").TLS(t)
	req.TLS.ClientCert = &stranger
	if res := c.Deliver(context.Background(), req); res.Outcome == OutcomeDelivered || len(rc.bodies) != 0 {
		t.Fatalf("with an untrusted client certificate: %+v", res)
	}
	cert := clientCA.Client(t, "open-kase").TLS(t)
	req.TLS.ClientCert = &cert
	if res := c.Deliver(context.Background(), req); res.Outcome != OutcomeDelivered || len(rc.bodies) != 1 {
		t.Fatalf("with the client certificate: %+v", res)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	max := time.Hour
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"30", 30 * time.Second, true},
		{"7200", time.Hour, true},
		{"99999999999999999", time.Hour, true},
		{"99999999999999999999999", 0, false},
		{"-5", 0, false},
		{"soon", 0, false},
		{"Fri, 02 Oct 2026 12:10:00 GMT", 10 * time.Minute, true},
		{"Fri, 02 Oct 2026 11:00:00 GMT", 0, true},
		{"Sat, 03 Oct 2026 12:00:00 GMT", time.Hour, true},
	}
	for _, tc := range cases {
		got, ok := ParseRetryAfter(tc.in, now, max)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q = %s %v, want %s %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDecideSchedule(t *testing.T) {
	identity := func(d time.Duration) time.Duration { return d }
	fail := Result{Outcome: OutcomeRetry}
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}
	for i, w := range want {
		d := Decide(i+1, fail, identity)
		if d.DeadLetter || d.Done || d.Delay != w {
			t.Errorf("attempt %d: %+v", i+1, d)
		}
	}
	if d := Decide(len(Schedule)+1, fail, identity); !d.DeadLetter {
		t.Errorf("after schedule: %+v", d)
	}
	if d := Decide(1, Result{Outcome: OutcomePermanent}, identity); !d.DeadLetter {
		t.Errorf("permanent: %+v", d)
	}
	if d := Decide(1, Result{Outcome: OutcomeDelivered}, identity); !d.Done {
		t.Errorf("delivered: %+v", d)
	}
	if d := Decide(1, Result{Outcome: OutcomeRetry, RetryAfter: 10 * time.Minute}, identity); d.Delay != 10*time.Minute {
		t.Errorf("retry-after: %+v", d)
	}
	if d := Decide(3, Result{Outcome: OutcomeRetry, RetryAfter: time.Minute}, identity); d.Delay != 30*time.Minute {
		t.Errorf("short retry-after shortened schedule: %+v", d)
	}
	for range 1000 {
		d := DefaultJitter(time.Minute)
		if d < 48*time.Second || d > 72*time.Second {
			t.Fatalf("jitter out of bounds: %s", d)
		}
	}
}
