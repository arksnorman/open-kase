package endpoints

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/webhooks"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Seal contexts bind a sealed value to its endpoint and purpose.
func headerContext(endpointID, name string) string {
	return "webhooks.endpoint:" + endpointID + ":header:" + strings.ToLower(name)
}
func passwordContext(endpointID string) string {
	return "webhooks.endpoint:" + endpointID + ":auth.password"
}
func tokenContext(endpointID string) string { return "webhooks.endpoint:" + endpointID + ":auth.token" }
func clientKeyContext(endpointID string) string {
	return "webhooks.endpoint:" + endpointID + ":tls.clientKey"
}

// HeaderInput is one header of a create or update. An omitted or empty
// Value keeps the stored value of the header with the same name
// (case-insensitive); a new header needs a value.
type HeaderInput struct {
	Name  string
	Value *string
}

// AuthInput replaces the authentication. For basic and token, an omitted or
// empty password/token keeps the stored one when the type is unchanged.
type AuthInput struct {
	Type     string
	Username *string
	Password *string
	Token    *string
}

// TLSInput changes certificate settings; nil fields are unchanged. An
// empty CACertificate or ClientCertificate removes it (removing the client
// certificate also removes its key). An omitted or empty ClientKey keeps
// the stored key, which must match the (new) client certificate.
type TLSInput struct {
	CACertificate     *string
	ClientCertificate *string
	ClientKey         *string
}

// CertInfo summarizes a certificate for administrators.
type CertInfo struct {
	Subject   string
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
}

// ParseCertificates parses every CERTIFICATE block of a PEM bundle. It
// fails on other block types, trailing garbage or an empty bundle.
func ParseCertificates(bundle string) ([]*x509.Certificate, error) {
	return validation.ParseCertificatePEM(bundle)
}

// Certificates summarizes a PEM bundle (nil when empty or invalid).
func Certificates(bundle string) []CertInfo {
	if bundle == "" {
		return nil
	}
	cs, err := ParseCertificates(bundle)
	if err != nil {
		return nil
	}
	out := make([]CertInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, CertInfo{Subject: c.Subject.String(), Issuer: c.Issuer.String(), NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC()})
	}
	return out
}

// pending is a plaintext waiting to be sealed after validation passed.
type pending struct {
	target  *string
	value   string
	context string
}

// applyRequest validates the header, auth and TLS parts of in against e and
// applies them to e. Plaintexts are sealed only after every check passed.
// It returns the changed field names.
func (s *Service) applyRequest(e *Endpoint, in Input, v *validation.Collector) ([]string, error) {
	var changes []string
	var seal []pending

	// Authentication.
	auth := e.Auth
	if auth.Type == "" {
		auth.Type = AuthNone
	}
	if a := in.Auth; a != nil {
		if fe := validation.WebhookAuthType.CheckString("auth.type", a.Type); fe != nil {
			v.Add(fe.Field, fe.Code, fe.Message)
		} else {
			prev := auth
			auth = Auth{Type: a.Type}
			switch a.Type {
			case AuthBasic:
				switch {
				case a.Username != nil:
					v.String(validation.WebhookAuthUsername, "auth.username", *a.Username)
					auth.Username = *a.Username
				case prev.Type == AuthBasic:
					auth.Username = prev.Username
				default:
					v.Add("auth.username", validation.CodeRequired, "Enter the username.")
				}
				switch {
				case a.Password != nil && *a.Password != "":
					v.String(validation.WebhookAuthPassword, "auth.password", *a.Password)
					seal = append(seal, pending{&auth.SealedPassword, *a.Password, passwordContext(e.Id)})
				case prev.Type == AuthBasic && prev.SealedPassword != "":
					auth.SealedPassword = prev.SealedPassword
				default:
					v.Add("auth.password", validation.CodeRequired, "Enter the password.")
				}
			case AuthToken:
				switch {
				case a.Token != nil && *a.Token != "":
					v.String(validation.WebhookAuthToken, "auth.token", *a.Token)
					seal = append(seal, pending{&auth.SealedToken, *a.Token, tokenContext(e.Id)})
				case prev.Type == AuthToken && prev.SealedToken != "":
					auth.SealedToken = prev.SealedToken
				default:
					v.Add("auth.token", validation.CodeRequired, "Enter the token.")
				}
			}
			if auth != prev || (a.Password != nil && *a.Password != "") || (a.Token != nil && *a.Token != "") {
				changes = append(changes, "auth")
			}
		}
	}
	authConfigured := auth.Type != AuthNone

	// Headers.
	headers := e.Headers
	if in.Headers != nil {
		hs := *in.Headers
		if len(hs) > validation.MaxWebhookHeaders {
			v.Add("headers", "tooMany", "Use at most "+strconv.Itoa(validation.MaxWebhookHeaders)+" headers.")
			hs = nil
		}
		stored := map[string]Header{}
		for _, h := range e.Headers {
			stored[strings.ToLower(h.Name)] = h
		}
		seen := map[string]bool{}
		headers = make([]Header, len(hs))
		for i, h := range hs {
			f := "headers[" + strconv.Itoa(i) + "]"
			if fe := validation.WebhookHeaderName.CheckString(f+".name", h.Name); fe != nil {
				v.Add(fe.Field, fe.Code, fe.Message)
				continue
			}
			lower := strings.ToLower(h.Name)
			if validation.WebhookHeaderReserved(h.Name, authConfigured) {
				v.Add(f+".name", "reserved", h.Name+" is set by open-kase and cannot be configured here.")
				continue
			}
			if seen[lower] {
				v.Add(f+".name", "duplicate", "Each header name can appear only once.")
				continue
			}
			seen[lower] = true
			headers[i].Name = h.Name
			if h.Value == nil || *h.Value == "" {
				if old, ok := stored[lower]; ok {
					headers[i].SealedValue = old.SealedValue
				} else {
					v.Add(f+".value", validation.CodeRequired, "Enter a value.")
				}
				continue
			}
			v.String(validation.WebhookHeaderValue, f+".value", *h.Value)
			seal = append(seal, pending{&headers[i].SealedValue, *h.Value, headerContext(e.Id, h.Name)})
		}
		if !headersEqual(headers, e.Headers) || len(seal) > 0 {
			changes = append(changes, "headers")
		}
	} else if authConfigured {
		for _, h := range e.Headers {
			if validation.WebhookHeaderReserved(h.Name, true) {
				v.Add("auth.type", "reserved", "Remove the "+h.Name+" header before choosing an authentication type.")
			}
		}
	}

	// TLS.
	t := e.TLS
	if ti := in.TLS; ti != nil {
		certChanged := false
		if ti.CACertificate != nil {
			ca := strings.TrimSpace(*ti.CACertificate)
			if ca != "" {
				v.String(validation.WebhookCACertificate, "tls.caCertificate", ca)
				if _, err := ParseCertificates(ca); err != nil {
					v.Add("tls.caCertificate", validation.CodeInvalidFormat, "Paste one or more PEM certificates (-----BEGIN CERTIFICATE-----).")
				}
			}
			t.CACertificate = ca
		}
		if ti.ClientCertificate != nil {
			c := strings.TrimSpace(*ti.ClientCertificate)
			if c == "" {
				t.ClientCertificate, t.SealedClientKey = "", ""
			} else if c != t.ClientCertificate {
				v.String(validation.WebhookClientCertificate, "tls.clientCertificate", c)
				if _, err := ParseCertificates(c); err != nil {
					v.Add("tls.clientCertificate", validation.CodeInvalidFormat, "Paste the client certificate in PEM format (-----BEGIN CERTIFICATE-----).")
				}
				t.ClientCertificate, certChanged = c, true
			}
		}
		newKey := ""
		if ti.ClientKey != nil {
			newKey = strings.TrimSpace(*ti.ClientKey)
		}
		if newKey != "" {
			v.String(validation.WebhookClientKey, "tls.clientKey", newKey)
			if t.ClientCertificate == "" {
				v.Add("tls.clientKey", "certificateRequired", "Add the client certificate that belongs to this key.")
			}
		}
		if t.ClientCertificate != "" && (certChanged || newKey != "") {
			key := newKey
			if key == "" && t.SealedClientKey != "" {
				plain, err := s.keys.Open(t.SealedClientKey, clientKeyContext(e.Id))
				if err != nil {
					return nil, errors.New("the stored client key cannot be decrypted with the current master key")
				}
				key = string(plain)
			}
			switch {
			case key == "":
				v.Add("tls.clientKey", validation.CodeRequired, "Paste the private key of the client certificate.")
			default:
				if _, err := tls.X509KeyPair([]byte(t.ClientCertificate), []byte(key)); err != nil {
					v.Add("tls.clientKey", validation.CodeMismatch, "The private key does not match the client certificate (or is not a PEM private key).")
				}
			}
			if newKey != "" {
				seal = append(seal, pending{&t.SealedClientKey, newKey, clientKeyContext(e.Id)})
			}
		}
		if t != e.TLS || newKey != "" {
			changes = append(changes, "tls")
		}
	}

	if err := v.Err(); err != nil {
		return nil, err
	}
	for _, p := range seal {
		sealed, err := s.keys.Seal([]byte(p.value), p.context)
		if err != nil {
			return nil, err
		}
		*p.target = sealed
	}
	e.Auth, e.Headers, e.TLS = auth, headers, t
	return changes, nil
}

func headersEqual(a, b []Header) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// errConfig marks stored configuration that cannot be used.
var errConfig = errors.New("the endpoint's stored credentials cannot be decrypted with the current master key")

// requestConfig opens the endpoint's sealed values for one delivery.
func requestConfig(keys Keys, ep Endpoint) (headers []webhooks.Header, authorization string, tlsCfg *webhooks.TLSConfig, err error) {
	open := func(sealed, context string) (string, error) {
		b, err := keys.Open(sealed, context)
		if err != nil {
			return "", errConfig
		}
		return string(b), nil
	}
	for _, h := range ep.Headers {
		val, err := open(h.SealedValue, headerContext(ep.Id, h.Name))
		if err != nil {
			return nil, "", nil, err
		}
		headers = append(headers, webhooks.Header{Name: h.Name, Value: val})
	}
	switch ep.Auth.Type {
	case AuthBasic:
		pw, err := open(ep.Auth.SealedPassword, passwordContext(ep.Id))
		if err != nil {
			return nil, "", nil, err
		}
		authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(ep.Auth.Username+":"+pw))
	case AuthToken:
		tok, err := open(ep.Auth.SealedToken, tokenContext(ep.Id))
		if err != nil {
			return nil, "", nil, err
		}
		authorization = "Bearer " + tok
	}
	if ep.TLS.CACertificate != "" || ep.TLS.ClientCertificate != "" {
		tlsCfg = &webhooks.TLSConfig{}
		if ep.TLS.CACertificate != "" {
			tlsCfg.CAPEM = []byte(ep.TLS.CACertificate)
		}
		if ep.TLS.ClientCertificate != "" {
			key, err := open(ep.TLS.SealedClientKey, clientKeyContext(ep.Id))
			if err != nil {
				return nil, "", nil, err
			}
			cert, err := tls.X509KeyPair([]byte(ep.TLS.ClientCertificate), []byte(key))
			if err != nil {
				return nil, "", nil, errors.New("the endpoint's client certificate and key cannot be used")
			}
			tlsCfg.ClientCert = &cert
		}
	}
	return headers, authorization, tlsCfg, nil
}
