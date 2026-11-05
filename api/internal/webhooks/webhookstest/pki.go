// Package webhookstest generates throwaway certificates for webhook TLS
// tests: a private CA, a server certificate for 127.0.0.1/localhost and a
// client certificate for mutual TLS. Test use only.
package webhookstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Pair is a certificate with its key in PEM and parsed forms.
type Pair struct {
	CertPEM string
	KeyPEM  string
	Cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// TLS returns the pair as a tls.Certificate.
func (p Pair) TLS(t testing.TB) tls.Certificate {
	t.Helper()
	c, err := tls.X509KeyPair([]byte(p.CertPEM), []byte(p.KeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var serial int64 = 1

func issue(t testing.TB, tmpl *x509.Certificate, parent *Pair) Pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial++
	tmpl.SerialNumber = big.NewInt(serial)
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	parentCert, parentKey := tmpl, key
	if parent != nil {
		parentCert, parentKey = parent.Cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentCert, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return Pair{
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})),
		Cert:    cert, key: key,
	}
}

// NewCA returns a self-signed CA.
func NewCA(t testing.TB, name string) Pair {
	return issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}, nil)
}

// Server returns a server certificate for 127.0.0.1 and localhost.
func (ca Pair) Server(t testing.TB) Pair {
	return issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, &ca)
}

// Client returns a client certificate.
func (ca Pair) Client(t testing.TB, name string) Pair {
	return issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: name}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, &ca)
}

// Server starts an HTTPS test server with a certificate from serverCA.
// With clientCA set the server requires a client certificate signed by it.
func Server(t testing.TB, serverCA Pair, clientCA *Pair, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	cfg := &tls.Config{Certificates: []tls.Certificate{serverCA.Server(t).TLS(t)}, MinVersion: tls.VersionTLS12}
	if clientCA != nil {
		pool := x509.NewCertPool()
		pool.AddCert(clientCA.Cert)
		cfg.ClientCAs, cfg.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}
