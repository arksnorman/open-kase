package mailer

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"mime"
	"net"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

// smtpServer is a minimal scripted SMTP server for adapter tests.
type smtpServer struct {
	t         *testing.T
	ln        net.Listener
	tlsCfg    *tls.Config
	roots     *x509.CertPool
	startTLS  bool // advertise STARTTLS
	implicit  bool // TLS from the first byte
	auth      bool // require AUTH PLAIN user/secret
	mailReply string
	rcptReply string
	endReply  string
	dropAtEnd bool // close the connection after the final "." without replying

	mu       sync.Mutex
	messages []received
}

type received struct {
	from, data, user string
	rcpts            []string
	tls              bool
}

func newSMTPServer(t *testing.T, configure func(*smtpServer)) *smtpServer {
	t.Helper()
	hs := httptest.NewTLSServer(nil) // borrow a certificate valid for 127.0.0.1
	t.Cleanup(hs.Close)
	roots := x509.NewCertPool()
	roots.AddCert(hs.Certificate())
	s := &smtpServer{t: t, tlsCfg: &tls.Config{Certificates: hs.TLS.Certificates, MinVersion: tls.VersionTLS12}, roots: roots}
	if configure != nil {
		configure(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if s.implicit {
		ln = tls.NewListener(ln, s.tlsCfg)
	}
	s.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpServer) received() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.messages...)
}

func (s *smtpServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	isTLS := s.implicit
	r := bufio.NewReader(c)
	w := func(line string) { _, _ = c.Write([]byte(line + "\r\n")) }
	w("220 test ESMTP")
	var cur received
	authed := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			w("250-test")
			if s.startTLS && !isTLS {
				w("250-STARTTLS")
			}
			if s.auth && isTLS {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case "STARTTLS":
			w("220 go ahead")
			tc := tls.Server(c, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, r, isTLS = tc, bufio.NewReader(tc), true
		case "AUTH":
			parts := strings.Fields(line)
			resp := ""
			if len(parts) == 3 {
				resp = parts[2]
			} else {
				w("334 ")
				l, _ := r.ReadString('\n')
				resp = strings.TrimSpace(l)
			}
			raw, _ := base64.StdEncoding.DecodeString(resp)
			f := strings.Split(string(raw), "\x00")
			if len(f) == 3 && f[1] == "user" && f[2] == "secret" {
				authed, cur.user = true, f[1]
				w("235 ok")
			} else {
				w("535 5.7.8 authentication failed")
			}
		case "NOOP", "RSET":
			w("250 ok")
		case "MAIL":
			if s.auth && !authed {
				w("530 auth required")
				continue
			}
			if s.mailReply != "" {
				w(s.mailReply)
				continue
			}
			cur.from = line
			w("250 ok")
		case "RCPT":
			if s.rcptReply != "" {
				w(s.rcptReply)
				continue
			}
			cur.rcpts = append(cur.rcpts, line)
			w("250 ok")
		case "DATA":
			w("354 send")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			if s.dropAtEnd {
				return
			}
			if s.endReply != "" {
				w(s.endReply)
				continue
			}
			cur.data, cur.tls = b.String(), isTLS
			s.mu.Lock()
			s.messages = append(s.messages, cur)
			s.mu.Unlock()
			cur = received{user: cur.user}
			w("250 queued")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("502 unknown")
		}
	}
}

func testMessage() Message {
	return Message{
		To:        Address{Name: "Ada Lovelace", Email: "ada@example.com"},
		Subject:   "[SUP-12] Printer offline",
		Text:      "Hello\n",
		HTML:      "<p>Hello</p>",
		MessageId: "evt-1.ada@support.example.com",
		Headers:   map[string]string{"X-Open-Kase-Event-Id": "evt-1"},
	}
}

func newAdapter(t *testing.T, s *smtpServer, mutate func(*Config)) *SMTP {
	t.Helper()
	cfg := Config{
		Host: "127.0.0.1", Port: s.port(), From: Address{Name: "Support", Email: "support@example.com"},
		TLSPolicy: TLSStartTLS, Timeout: 5 * time.Second, RootCAs: s.roots, HELO: "client.test",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := NewSMTP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func wantClass(t *testing.T, err error, class Class, phase Phase) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not *Error", err)
	}
	if e.Class != class || e.Phase != phase {
		t.Fatalf("got class=%s phase=%s (%v), want %s/%s", e.Class, e.Phase, err, class, phase)
	}
	if ClassOf(err) != class {
		t.Fatalf("ClassOf = %s", ClassOf(err))
	}
	return e
}

func TestSMTPStartTLSWithAuth(t *testing.T) {
	s := newSMTPServer(t, func(s *smtpServer) { s.startTLS, s.auth = true, true })
	m := newAdapter(t, s, func(c *Config) { c.Username, c.Password = "user", "secret" })
	if err := m.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	got := s.received()
	if len(got) != 1 {
		t.Fatalf("received %d messages", len(got))
	}
	r := got[0]
	if !r.tls || r.user != "user" || len(r.rcpts) != 1 || !strings.Contains(r.rcpts[0], "<ada@example.com>") {
		t.Fatalf("envelope = %+v", r)
	}
	for _, want := range []string{
		"Message-ID: <evt-1.ada@support.example.com>",
		"Auto-Submitted: auto-generated",
		"X-Open-Kase-Event-Id: evt-1",
		"Subject: [SUP-12] Printer offline",
		`To: "Ada Lovelace" <ada@example.com>`,
		`From: "Support" <support@example.com>`,
		"multipart/alternative",
		"text/plain",
		"text/html",
	} {
		if !strings.Contains(r.data, want) {
			t.Errorf("message lacks %q:\n%s", want, r.data)
		}
	}
	if strings.Contains(r.data, "secret") {
		t.Fatal("password leaked into message")
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	s := newSMTPServer(t, func(s *smtpServer) { s.implicit, s.auth = true, true })
	m := newAdapter(t, s, func(c *Config) { c.TLSPolicy, c.Username, c.Password = TLSImplicit, "user", "secret" })
	if err := m.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if got := s.received(); len(got) != 1 || !got[0].tls {
		t.Fatalf("received = %+v", got)
	}
}

func TestSMTPMandatoryStartTLSMissing(t *testing.T) {
	s := newSMTPServer(t, nil)
	err := newAdapter(t, s, nil).Send(context.Background(), testMessage())
	wantClass(t, err, ClassPermanent, PhaseConnect)
	if len(s.received()) != 0 {
		t.Fatal("sent without TLS")
	}
}

func TestSMTPOpportunisticFallsBackToPlain(t *testing.T) {
	s := newSMTPServer(t, nil)
	m := newAdapter(t, s, func(c *Config) { c.TLSPolicy = TLSStartTLSOpportunistic })
	if err := m.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if got := s.received(); len(got) != 1 || got[0].tls {
		t.Fatalf("received = %+v", got)
	}
}

func TestSMTPOpportunisticUsesTLSWhenOffered(t *testing.T) {
	s := newSMTPServer(t, func(s *smtpServer) { s.startTLS = true })
	m := newAdapter(t, s, func(c *Config) { c.TLSPolicy = TLSStartTLSOpportunistic })
	if err := m.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if got := s.received(); len(got) != 1 || !got[0].tls {
		t.Fatalf("received = %+v", got)
	}
}

func TestSMTPUntrustedCertificateIsPermanent(t *testing.T) {
	s := newSMTPServer(t, func(s *smtpServer) { s.startTLS = true })
	m := newAdapter(t, s, func(c *Config) { c.RootCAs = x509.NewCertPool() })
	wantClass(t, m.Send(context.Background(), testMessage()), ClassPermanent, PhaseConnect)
	if len(s.received()) != 0 {
		t.Fatal("sent over unverified TLS")
	}
}

func TestSMTPAuthFailureIsPermanent(t *testing.T) {
	s := newSMTPServer(t, func(s *smtpServer) { s.startTLS, s.auth = true, true })
	m := newAdapter(t, s, func(c *Config) { c.Username, c.Password = "user", "wrong-password" })
	err := m.Send(context.Background(), testMessage())
	e := wantClass(t, err, ClassPermanent, PhaseConnect)
	if e.Code != 535 {
		t.Fatalf("code = %d", e.Code)
	}
	if strings.Contains(err.Error(), "wrong-password") {
		t.Fatal("password in error")
	}
}

func TestSMTPReplyClassification(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*smtpServer)
		class Class
		phase Phase
		code  int
	}{
		{"mail 451", func(s *smtpServer) { s.mailReply = "451 4.3.0 try later" }, ClassTemporary, PhaseEnvelope, 451},
		{"rcpt 550", func(s *smtpServer) { s.rcptReply = "550 5.1.1 no such user" }, ClassPermanent, PhaseEnvelope, 550},
		{"rcpt 452", func(s *smtpServer) { s.rcptReply = "452 4.2.2 mailbox full" }, ClassTemporary, PhaseEnvelope, 452},
		{"end 554", func(s *smtpServer) { s.endReply = "554 5.6.0 rejected" }, ClassPermanent, PhaseCommit, 554},
		{"end 451", func(s *smtpServer) { s.endReply = "451 4.3.0 local error" }, ClassTemporary, PhaseCommit, 451},
		{"dropped after data", func(s *smtpServer) { s.dropAtEnd = true }, ClassUnknownOutcome, PhaseCommit, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSMTPServer(t, func(s *smtpServer) { s.startTLS = true; tc.setup(s) })
			e := wantClass(t, newAdapter(t, s, nil).Send(context.Background(), testMessage()), tc.class, tc.phase)
			if e.Code != tc.code {
				t.Fatalf("code = %d, want %d", e.Code, tc.code)
			}
		})
	}
}

func TestSMTPConnectionRefusedIsTemporary(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	m, err := NewSMTP(Config{Host: "127.0.0.1", Port: port, From: Address{Email: "support@example.com"}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	wantClass(t, m.Send(context.Background(), testMessage()), ClassTemporary, PhaseConnect)
}

// OC-ASYNC-02: display names are limited in code points (contact.name
// allows 200), not bytes. A long non-ASCII name must be sent, as RFC 2047
// encoded words on folded lines, and decode back to the same name.
func TestSMTPLongNonASCIIDisplayName(t *testing.T) {
	s := newSMTPServer(t, func(s *smtpServer) { s.startTLS = true })
	name := strings.Repeat("Жанна ", 33) + "Ж!" // 200 code points, 365 bytes
	if n := len([]rune(name)); n != MaxNameRunes {
		t.Fatalf("fixture has %d code points", n)
	}
	msg := testMessage()
	msg.To.Name = name
	msg.Subject = "[SUP-12] " + strings.Repeat("ю", 240)
	if err := newAdapter(t, s, nil).Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	got := s.received()
	if len(got) != 1 {
		t.Fatalf("received %d messages", len(got))
	}
	parsed, err := mail.ReadMessage(strings.NewReader(got[0].data))
	if err != nil {
		t.Fatal(err)
	}
	to, err := parsed.Header.AddressList("To")
	if err != nil || len(to) != 1 || to[0].Name != name || to[0].Address != "ada@example.com" {
		t.Fatalf("To header decodes to %+v (%v)", to, err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil || subject != msg.Subject {
		t.Fatalf("Subject decodes to %q (%v)", subject, err)
	}
	for _, line := range strings.Split(strings.SplitN(got[0].data, "\r\n\r\n", 2)[0], "\r\n") {
		if len(line) > 998 {
			t.Fatalf("header line of %d bytes exceeds the RFC 5322 limit", len(line))
		}
	}
	// One code point more is refused before connecting.
	msg.To.Name = name + "x"
	wantClass(t, newAdapter(t, s, nil).Send(context.Background(), msg), ClassPermanent, PhaseValidate)
}

func TestSMTPRejectsInvalidMessageBeforeConnecting(t *testing.T) {
	m, err := NewSMTP(Config{Host: "127.0.0.1", Port: 1, From: Address{Email: "support@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	msg := testMessage()
	msg.Subject = "hi\r\nBcc: victim@example.com"
	wantClass(t, m.Send(context.Background(), msg), ClassPermanent, PhaseValidate)
}

func TestConfigValidate(t *testing.T) {
	base := Config{Host: "smtp.example.com", Port: 587, From: Address{Email: "support@example.com"}, TLSPolicy: TLSStartTLS}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"auth without tls":         func(c *Config) { c.Username, c.TLSPolicy = "u", TLSNone },
		"auth with opportunistic":  func(c *Config) { c.Username, c.TLSPolicy = "u", TLSStartTLSOpportunistic },
		"password without user":    func(c *Config) { c.Password = "p" },
		"bad port":                 func(c *Config) { c.Port = 0 },
		"no host":                  func(c *Config) { c.Host = "" },
		"bad from":                 func(c *Config) { c.From.Email = "a@b, c@d" },
		"unknown policy":           func(c *Config) { c.TLSPolicy = "ssl3" },
		"header injection in name": func(c *Config) { c.From.Name = "x\nBcc: y@z" },
	}
	for name, mutate := range bad {
		c := base
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for in, want := range map[string]TLSPolicy{"": TLSStartTLS, "starttls": TLSStartTLS, "tls": TLSImplicit, "none": TLSNone} {
		if got, err := ParseTLSPolicy(in); err != nil || got != want {
			t.Errorf("ParseTLSPolicy(%q) = %q, %v", in, got, err)
		}
	}
}

func TestMessageValidate(t *testing.T) {
	if err := testMessage().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Message){
		"two recipients":       func(m *Message) { m.To.Email = "a@example.com, b@example.com" },
		"display form in addr": func(m *Message) { m.To.Email = "Eve <eve@example.com>" },
		"newline in name":      func(m *Message) { m.To.Name = "Ada\r\nBcc: x@y" },
		"newline in subject":   func(m *Message) { m.Subject = "a\nb" },
		"empty subject":        func(m *Message) { m.Subject = " " },
		"long subject":         func(m *Message) { m.Subject = strings.Repeat("a", MaxSubjectRunes+1) },
		"no text":              func(m *Message) { m.Text = "" },
		"bracketed id":         func(m *Message) { m.MessageId = "<a@b>" },
		"structural header":    func(m *Message) { m.Headers = map[string]string{"Bcc": "x@y"} },
		"header injection":     func(m *Message) { m.Headers = map[string]string{"X-A": "1\r\nBcc: x@y"} },
		"bad reply-to":         func(m *Message) { m.ReplyTo = &Address{Email: "nope"} },
		"invalid utf8":         func(m *Message) { m.Text = "\xff" },
	}
	for name, mutate := range bad {
		m := testMessage()
		mutate(&m)
		err := m.Validate()
		if !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestFakeMailer(t *testing.T) {
	var f Fake
	msg := testMessage()
	if err := f.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	sent := f.Sent()
	sent[0].Headers["X-Open-Kase-Event-Id"] = "mutated"
	if f.Sent()[0].Headers["X-Open-Kase-Event-Id"] != "evt-1" {
		t.Fatal("Sent exposes internal state")
	}
	f.FailWith = func(m Message) error {
		return &Error{Class: ClassTemporary, Phase: PhaseConnect, Err: errors.New("down")}
	}
	if ClassOf(f.Send(context.Background(), msg)) != ClassTemporary {
		t.Fatal("FailWith ignored")
	}
	bad := msg
	bad.Subject = "a\nb"
	f.FailWith = nil
	if ClassOf(f.Send(context.Background(), bad)) != ClassPermanent {
		t.Fatal("fake accepted an invalid message")
	}
	if len(f.Sent()) != 1 {
		t.Fatalf("sent = %d", len(f.Sent()))
	}
	f.Reset()
	if len(f.Sent()) != 0 {
		t.Fatal("reset")
	}
	if ClassOf(errors.New("foreign")) != ClassUnknownOutcome || ClassOf(nil) != "" {
		t.Fatal("ClassOf defaults")
	}
}
