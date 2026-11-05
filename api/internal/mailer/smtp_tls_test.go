package mailer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"testing"
)

func TestSMTPSkipVerifyAcceptsUntrustedCertificate(t *testing.T) {
	s := newSMTPServer(t, func(s *smtpServer) { s.startTLS = true })
	m := newAdapter(t, s, func(c *Config) { c.RootCAs, c.InsecureSkipVerify = x509.NewCertPool(), true })
	if err := m.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if got := s.received(); len(got) != 1 || !got[0].tls {
		t.Fatalf("received = %+v", got)
	}
}

func TestConfigTLSConfig(t *testing.T) {
	pool := x509.NewCertPool()
	cfg := Config{Host: "relay.example.com", TLSPolicy: TLSStartTLS, RootCAs: pool, InsecureSkipVerify: true}.tlsConfig()
	if cfg.RootCAs != pool || !cfg.InsecureSkipVerify || cfg.MinVersion != tls.VersionTLS12 || cfg.ServerName != "relay.example.com" {
		t.Fatalf("tls config = %+v", cfg)
	}
	cfg = Config{Host: "relay.example.com", TLSPolicy: TLSStartTLS}.tlsConfig()
	if cfg.RootCAs != nil || cfg.InsecureSkipVerify {
		t.Fatalf("defaults must verify with system roots: %+v", cfg)
	}
	cfg = Config{Host: "relay.example.com", TLSPolicy: TLSNone, RootCAs: pool, InsecureSkipVerify: true}.tlsConfig()
	if cfg.RootCAs != nil || cfg.InsecureSkipVerify {
		t.Fatalf("none must ignore CA and skip-verify: %+v", cfg)
	}
}

func TestRootsWithPEM(t *testing.T) {
	hs := httptest.NewTLSServer(nil)
	defer hs.Close()
	bundle := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: hs.Certificate().Raw}))
	roots, err := RootsWithPEM(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.Certificate().Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Fatalf("custom CA not trusted: %v", err)
	}
	if _, err := RootsWithPEM("garbage"); err == nil {
		t.Fatal("garbage accepted")
	}
}
