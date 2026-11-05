package validation

import (
	"regexp"
	"slices"
	"strings"
)

// Webhook request configuration: extra headers, authentication and TLS
// (owner decision 2026-10-31; replaces HMAC signing).
var (
	// WebhookHeaderName is an RFC 9110 field name (token characters).
	WebhookHeaderName = define(Rule{Field: "webhook.header.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 100,
		Pattern: regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$"),
		Message: "Use letters, digits and - (an HTTP header name)."})
	// WebhookHeaderValue may be a secret; CR, LF and other control
	// characters are rejected.
	WebhookHeaderValue = define(Rule{Field: "webhook.header.value", Kind: KindSecret, Required: true, MinLen: 1, MaxLen: 4096})
	WebhookAuthType    = define(Rule{Field: "webhook.auth.type", Kind: KindEnum, Required: true, Choices: []string{"none", "basic", "token"},
		Message: "Choose none, basic or token."})
	WebhookAuthUsername = define(Rule{Field: "webhook.auth.username", Kind: KindText, Required: true, MinLen: 1, MaxLen: 256,
		Pattern: regexp.MustCompile(`^[^:]+$`), Message: "Use up to 256 characters without ':'."})
	WebhookAuthPassword      = define(Rule{Field: "webhook.auth.password", Kind: KindSecret, Required: true, MinLen: 1, MaxLen: 1024})
	WebhookAuthToken         = define(Rule{Field: "webhook.auth.token", Kind: KindSecret, Required: true, MinLen: 1, MaxLen: 4096})
	WebhookCACertificate     = define(Rule{Field: "webhook.tls.caCertificate", Kind: KindText, MaxLen: 65536, Multiline: true})
	WebhookClientCertificate = define(Rule{Field: "webhook.tls.clientCertificate", Kind: KindText, MaxLen: 32768, Multiline: true})
	WebhookClientKey         = define(Rule{Field: "webhook.tls.clientKey", Kind: KindSecret, MaxLen: 32768, Multiline: true})
)

// WebhookName is the endpoint's display name.
var WebhookName = define(Rule{Field: "webhook.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 80})

// MaxWebhookHeaders bounds an endpoint's extra headers.
const MaxWebhookHeaders = 20

// reservedWebhookHeaders are set by the sender or the HTTP layer.
var reservedWebhookHeaders = []string{
	"host", "content-length", "content-type", "transfer-encoding", "connection", "keep-alive", "proxy-connection",
	"proxy-authorization", "te", "trailer", "upgrade", "expect", "x-open-kase-event-id", "x-open-kase-event-type",
}

// WebhookHeaderReserved reports whether an extra header may not use name.
// Authorization is reserved while the endpoint uses Basic or token auth.
func WebhookHeaderReserved(name string, authConfigured bool) bool {
	n := strings.ToLower(name)
	if n == "authorization" {
		return authConfigured
	}
	return slices.Contains(reservedWebhookHeaders, n)
}
