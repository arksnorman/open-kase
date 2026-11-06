// Package settings owns the persisted, versioned installation configuration
// and the immutable typed snapshot that runtime services read.
package settings

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/arksnorman/open-kase/api/package/product"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Settings is the installation-level product configuration stored in MongoDB.
type Settings struct {
	Product ProductSettings
	Auth    AuthSettings
	Files   FileSettings
	SMTP    SMTPSettings
	// Notifications controls requester-facing email content.
	Notifications NotificationSettings
}

// NotificationSettings controls the emails sent to requester contacts.
type NotificationSettings struct {
	// RequesterLinkBaseURL is the base of the link in requester emails:
	// <base>/<ticket number> opens the ticket in the system that submitted it
	// (never the agent UI). Empty: requester emails carry no link and ask
	// the requester to check the system they used.
	RequesterLinkBaseURL string
	// RequesterEmailsOff stops all email to contacts and CC addresses
	// (stored inverted so the zero value keeps the default: on). Staff
	// emails are unaffected. Use RequesterEmailsEnabled.
	RequesterEmailsOff bool
}

// RequesterEmailsEnabled reports whether contacts and CC addresses are emailed.
func (n NotificationSettings) RequesterEmailsEnabled() bool { return !n.RequesterEmailsOff }

// RequesterTicketURL returns the requester link for a ticket number
// (<base>/<number>), or "" when no requester link base URL is configured.
func (n NotificationSettings) RequesterTicketURL(number int64) string {
	base := strings.TrimRight(strings.TrimSpace(n.RequesterLinkBaseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/" + strconv.FormatInt(number, 10)
}

// ProductSettings controls public identity and links.
type ProductSettings struct {
	Name string
	// Description is optional free text about this installation (shown on
	// the Configuration page).
	Description string
	// Signature is the optional Markdown workspace signature the server
	// appends to staff public replies.
	Signature string
	BaseURL   string // agent application URL used in staff emails; empty until configured
}

// AuthSettings controls token lifetimes and administrator MFA.
type AuthSettings struct {
	AccessTokenTTLSeconds int64
	RefreshTokenTTLHours  int64
	// RequireAdminMFA forces administrators without a TOTP
	// authenticator to enrol one before any other API use. Enable it
	// before exposing the installation to the internet.
	RequireAdminMFA bool
}

// FileSettings bounds attachments and message sizes.
type FileSettings struct {
	MaxAttachmentBytes       int64
	MaxAttachmentsPerMessage int64
	MaxMessageTotalBytes     int64
	MaxMessageCodePoints     int64
}

// SMTPSettings configures outbound email. PasswordCiphertext is sealed with
// the master key and is never returned by redacted views.
type SMTPSettings struct {
	Enabled            bool
	Host               string
	Port               int64
	Username           string
	PasswordCiphertext string
	FromAddress        string
	FromName           string
	TLSMode            string // starttls, tls or none
	// CACertificate is an optional PEM bundle trusted in addition to the
	// system roots (not secret). TLSSkipVerify disables server certificate
	// verification. Neither applies when TLSMode is none.
	CACertificate string
	TLSSkipVerify bool
}

// SMTPPasswordContext is the authenticated-encryption context for the SMTP password.
const SMTPPasswordContext = "settings.smtp.password"

// Defaults returns built-in defaults (the lowest precedence layer).
func Defaults() Settings {
	return Settings{
		Product: ProductSettings{Name: product.Name},
		Auth:    AuthSettings{AccessTokenTTLSeconds: 600, RefreshTokenTTLHours: 14 * 24},
		Files: FileSettings{
			MaxAttachmentBytes:       10 << 20,
			MaxAttachmentsPerMessage: 5,
			MaxMessageTotalBytes:     50 << 20,
			MaxMessageCodePoints:     100_000,
		},
		SMTP: SMTPSettings{Port: 587, TLSMode: "starttls"},
	}
}

// Validate checks every field against the shared validation catalog. It is
// applied to persisted settings on load as well as to updates.
func (s Settings) Validate() error {
	var v validation.Collector
	v.String(validation.SettingsProductName, "product.name", s.Product.Name)
	v.String(validation.SettingsProductDescription, "product.description", s.Product.Description)
	v.String(validation.SettingsProductSignature, "product.signature", s.Product.Signature)
	v.String(validation.SettingsBaseURL, "product.baseUrl", s.Product.BaseURL)
	v.Int(validation.SettingsAccessTokenTTL, "auth.accessTokenTtlSeconds", s.Auth.AccessTokenTTLSeconds)
	v.Int(validation.SettingsRefreshTokenTTL, "auth.refreshTokenTtlHours", s.Auth.RefreshTokenTTLHours)
	v.Int(validation.SettingsMaxAttachment, "files.maxAttachmentBytes", s.Files.MaxAttachmentBytes)
	v.Int(validation.SettingsMaxAttachments, "files.maxAttachmentsPerMessage", s.Files.MaxAttachmentsPerMessage)
	v.Int(validation.SettingsMaxMessageBytes, "files.maxMessageTotalBytes", s.Files.MaxMessageTotalBytes)
	v.Int(validation.SettingsMaxMessageChars, "files.maxMessageCodePoints", s.Files.MaxMessageCodePoints)
	if s.Files.MaxAttachmentBytes > s.Files.MaxMessageTotalBytes {
		v.Add("files.maxAttachmentBytes", validation.CodeOutOfRange, "A single attachment cannot exceed the per-message total.")
	}
	v.String(validation.SettingsSMTPHost, "smtp.host", s.SMTP.Host)
	v.Int(validation.SettingsSMTPPort, "smtp.port", s.SMTP.Port)
	v.String(validation.SettingsSMTPUsername, "smtp.username", s.SMTP.Username)
	v.String(validation.SettingsSMTPFromAddress, "smtp.fromAddress", s.SMTP.FromAddress)
	v.String(validation.SettingsSMTPFromName, "smtp.fromName", s.SMTP.FromName)
	v.String(validation.SettingsSMTPTLSMode, "smtp.tlsMode", s.SMTP.TLSMode)
	if s.SMTP.Enabled {
		if s.SMTP.Host == "" {
			v.Add("smtp.host", validation.CodeRequired, "A host is required when email is enabled.")
		}
		if s.SMTP.FromAddress == "" {
			v.Add("smtp.fromAddress", validation.CodeRequired, "A sender address is required when email is enabled.")
		}
		if s.Product.BaseURL == "" {
			v.Add("product.baseUrl", validation.CodeRequired, "A base URL is required so emails can link to the portal.")
		}
	}
	if ca := strings.TrimSpace(s.SMTP.CACertificate); ca != "" {
		v.String(validation.SettingsSMTPCACertificate, "smtp.caCertificate", ca)
		if _, err := validation.ParseCertificatePEM(ca); err != nil {
			v.Add("smtp.caCertificate", validation.CodeInvalidFormat, "Paste one or more PEM certificates (-----BEGIN CERTIFICATE-----).")
		}
	}
	if s.SMTP.Username != "" && s.SMTP.TLSMode == "none" {
		v.Add("smtp.tlsMode", validation.CodeInvalidChoice, "Authenticated SMTP requires TLS.")
	}
	const linkField = "notifications.requesterLinkBaseUrl"
	if raw := s.Notifications.RequesterLinkBaseURL; raw != "" {
		switch {
		case validation.SettingsRequesterLinkBaseURL.CheckString(linkField, raw) != nil:
			v.String(validation.SettingsRequesterLinkBaseURL, linkField, raw)
		case strings.ContainsAny(raw, "?#") || strings.ContainsFunc(raw, unicode.IsSpace):
			v.Add(linkField, validation.CodeInvalidFormat, "Enter a base URL without spaces, a query or a fragment; the ticket number is appended as /<number>.")
		case s.Product.BaseURL != "" && underBase(raw, strings.TrimRight(s.Product.BaseURL, "/")):
			v.Add(linkField, validation.CodeInvalidFormat, "Requesters have no login: use the address of the system that submits tickets, not this application's base URL.")
		}
	}
	return v.Err()
}

// underBase reports whether raw is base itself or a path below it (compared
// case-insensitively for scheme and host).
func underBase(raw, base string) bool {
	r, b := strings.ToLower(strings.TrimRight(raw, "/")), strings.ToLower(base)
	return r == b || strings.HasPrefix(r, b+"/")
}

// Redacted is the settings representation safe for administrators to read.
// Secrets are replaced by a configured flag.
type Redacted struct {
	Version   int64           `json:"configVersion"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Product   ProductSettings `json:"product"`
	Auth      AuthSettings    `json:"auth"`
	Files     FileSettings    `json:"files"`
	SMTP      RedactedSMTP    `json:"smtp"`
	// Notifications holds the requester link base URL.
	Notifications NotificationSettings `json:"notifications"`
}

// RedactedSMTP omits the password ciphertext.
type RedactedSMTP struct {
	Enabled            bool   `json:"enabled"`
	Host               string `json:"host"`
	Port               int64  `json:"port"`
	Username           string `json:"username"`
	PasswordConfigured bool   `json:"passwordConfigured"`
	FromAddress        string `json:"fromAddress"`
	FromName           string `json:"fromName"`
	TLSMode            string `json:"tlsMode"`
	CACertificate      string `json:"caCertificate"`
	TLSSkipVerify      bool   `json:"tlsSkipVerify"`
}

// Diff returns the dotted field paths that differ between a and b. Secret
// fields are reported by name only.
func Diff(a, b Settings) []string {
	var out []string
	add := func(changed bool, path string) {
		if changed {
			out = append(out, path)
		}
	}
	add(a.Product.Name != b.Product.Name, "product.name")
	add(a.Product.Description != b.Product.Description, "product.description")
	add(a.Product.Signature != b.Product.Signature, "product.signature")
	add(a.Product.BaseURL != b.Product.BaseURL, "product.baseUrl")
	add(a.Auth.AccessTokenTTLSeconds != b.Auth.AccessTokenTTLSeconds, "auth.accessTokenTtlSeconds")
	add(a.Auth.RefreshTokenTTLHours != b.Auth.RefreshTokenTTLHours, "auth.refreshTokenTtlHours")
	add(a.Auth.RequireAdminMFA != b.Auth.RequireAdminMFA, "auth.requireAdminMfa")
	add(a.Files.MaxAttachmentBytes != b.Files.MaxAttachmentBytes, "files.maxAttachmentBytes")
	add(a.Files.MaxAttachmentsPerMessage != b.Files.MaxAttachmentsPerMessage, "files.maxAttachmentsPerMessage")
	add(a.Files.MaxMessageTotalBytes != b.Files.MaxMessageTotalBytes, "files.maxMessageTotalBytes")
	add(a.Files.MaxMessageCodePoints != b.Files.MaxMessageCodePoints, "files.maxMessageCodePoints")
	add(a.SMTP.Enabled != b.SMTP.Enabled, "smtp.enabled")
	add(a.SMTP.Host != b.SMTP.Host, "smtp.host")
	add(a.SMTP.Port != b.SMTP.Port, "smtp.port")
	add(a.SMTP.Username != b.SMTP.Username, "smtp.username")
	add(a.SMTP.PasswordCiphertext != b.SMTP.PasswordCiphertext, "smtp.password")
	add(a.SMTP.FromAddress != b.SMTP.FromAddress, "smtp.fromAddress")
	add(a.SMTP.FromName != b.SMTP.FromName, "smtp.fromName")
	add(a.SMTP.TLSMode != b.SMTP.TLSMode, "smtp.tlsMode")
	add(a.SMTP.CACertificate != b.SMTP.CACertificate, "smtp.caCertificate")
	add(a.SMTP.TLSSkipVerify != b.SMTP.TLSSkipVerify, "smtp.tlsSkipVerify")
	add(a.Notifications.RequesterLinkBaseURL != b.Notifications.RequesterLinkBaseURL, "notifications.requesterLinkBaseUrl")
	add(a.Notifications.RequesterEmailsOff != b.Notifications.RequesterEmailsOff, "notifications.requesterEmails")
	return out
}
