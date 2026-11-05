package settings

import (
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSMTPCACertificateValidation(t *testing.T) {
	hs := httptest.NewTLSServer(nil)
	defer hs.Close()
	good := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: hs.Certificate().Raw}))
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}))
	for name, tc := range map[string]struct {
		pem string
		ok  bool
	}{
		"empty":    {"", true},
		"valid":    {good, true},
		"bundle":   {good + good, true},
		"garbage":  {"not a certificate", false},
		"key":      {key, false},
		"trailing": {good + "junk", false},
		"too big":  {good + strings.Repeat("#", 70000), false},
	} {
		s := Defaults()
		s.SMTP.CACertificate = tc.pem
		if err := s.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSMTPTLSDiffAndDestination(t *testing.T) {
	a := Defaults()
	b := a
	b.SMTP.CACertificate, b.SMTP.TLSSkipVerify = "pem", true
	got := strings.Join(Diff(a, b), ",")
	if !strings.Contains(got, "smtp.caCertificate") || !strings.Contains(got, "smtp.tlsSkipVerify") {
		t.Fatalf("diff = %s", got)
	}
	if !smtpDestinationChanged(a.SMTP, b.SMTP) {
		t.Fatal("skip-verify or a new CA must count as a destination change")
	}
	c := a
	c.SMTP.CACertificate = "pem"
	if !smtpDestinationChanged(a.SMTP, c.SMTP) {
		t.Fatal("a new CA must count as a destination change")
	}
	d := a
	d.SMTP.TLSSkipVerify = true
	if !smtpDestinationChanged(a.SMTP, d.SMTP) || smtpDestinationChanged(d.SMTP, a.SMTP) {
		t.Fatal("only turning skip-verify on is a destination change")
	}
	if smtpDestinationChanged(c.SMTP, a.SMTP) {
		t.Fatal("removing the CA must not need the password")
	}
}
