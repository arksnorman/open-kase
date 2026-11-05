package validation

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
)

// ParseCertificatePEM parses every CERTIFICATE block of a PEM bundle. It
// fails on other block types, trailing garbage or an empty bundle.
func ParseCertificatePEM(bundle string) ([]*x509.Certificate, error) {
	rest := []byte(bundle)
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, errors.New("only CERTIFICATE blocks are allowed")
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("not a PEM certificate")
	}
	return out, nil
}
