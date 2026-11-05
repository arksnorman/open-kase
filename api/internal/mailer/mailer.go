// Package mailer sends outbound notification email. Notification workers
// depend on the small Mailer interface; the SMTP adapter (smtp.go) is built
// on github.com/wneessen/go-mail and the Fake (fake.go) records messages for
// tests.
//
// Every Message has exactly one recipient so addresses are never exposed in
// a shared To/CC list. Send is called outside database transactions and is
// at-least-once: a retried job may deliver a duplicate, which is why each
// Message carries a stable Message-ID and why errors distinguish an unknown
// outcome from a failure that certainly did not deliver.
package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Mailer delivers one message to one recipient.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// Address is a display name and an email address.
type Address struct {
	Name  string
	Email string
}

// Message is one email to one recipient. Text is mandatory (plain-text
// alternative); HTML is optional. MessageID is the stable RFC 5322 msg-id
// without angle brackets ("<id>@<domain>"), reused on retries.
type Message struct {
	To        Address
	ReplyTo   *Address
	Subject   string
	Text      string
	HTML      string
	MessageId string
	// Headers are additional header fields. Only X-* fields and the names in
	// allowedHeaders are accepted; structural headers cannot be overridden.
	Headers map[string]string
}

// Limits for message fields.
const (
	MaxSubjectRunes = 255
	// MaxNameRunes bounds a display name (templates.Line cuts names to it).
	MaxNameRunes   = 200
	MaxBodyBytes   = 1 << 20
	maxHeaderValue = 998
)

var (
	headerNameRE = regexp.MustCompile(`^X-[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)
	msgIDRE      = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+/=?^_{|}~.-]{1,128}@[A-Za-z0-9.-]{1,253}$`)
)

// allowedHeaders are non-X headers that notifications may set.
var allowedHeaders = map[string]bool{
	"Auto-Submitted":           true,
	"List-Id":                  true,
	"List-Unsubscribe":         true,
	"List-Unsubscribe-Post":    true,
	"In-Reply-To":              true,
	"References":               true,
	"X-Auto-Response-Suppress": true,
}

// ErrInvalidMessage is wrapped by Validate errors.
var ErrInvalidMessage = errors.New("invalid email message")

// Validate rejects messages that could inject headers, address several
// recipients or exceed size limits.
func (m Message) Validate() error {
	if err := validateAddress("to", m.To); err != nil {
		return err
	}
	if m.ReplyTo != nil {
		if err := validateAddress("replyTo", *m.ReplyTo); err != nil {
			return err
		}
	}
	if strings.TrimSpace(m.Subject) == "" {
		return invalid("subject is required")
	}
	if hasLineBreakOrControl(m.Subject) || utf8.RuneCountInString(m.Subject) > MaxSubjectRunes {
		return invalid("subject must be one line of at most %d characters", MaxSubjectRunes)
	}
	if strings.TrimSpace(m.Text) == "" {
		return invalid("a plain-text body is required")
	}
	if len(m.Text) > MaxBodyBytes || len(m.HTML) > MaxBodyBytes {
		return invalid("body exceeds %d bytes", MaxBodyBytes)
	}
	if !utf8.ValidString(m.Text) || !utf8.ValidString(m.HTML) {
		return invalid("body is not valid UTF-8")
	}
	if m.MessageId != "" && !msgIDRE.MatchString(m.MessageId) {
		return invalid("message id must look like local@domain without angle brackets")
	}
	for name, value := range m.Headers {
		if !headerNameRE.MatchString(name) && !allowedHeaders[name] {
			return invalid("header %q is not allowed", name)
		}
		if hasLineBreakOrControl(value) || len(value) > maxHeaderValue {
			return invalid("header %q has an invalid value", name)
		}
	}
	return nil
}

func validateAddress(field string, a Address) error {
	if a.Email == "" {
		return invalid("%s address is required", field)
	}
	if hasLineBreakOrControl(a.Email) || hasLineBreakOrControl(a.Name) {
		return invalid("%s contains invalid characters", field)
	}
	// Code points, like the validation catalog (contact.name allows 200):
	// a byte limit rejected valid non-ASCII names for good.
	if utf8.RuneCountInString(a.Name) > MaxNameRunes {
		return invalid("%s display name exceeds %d characters", field, MaxNameRunes)
	}
	parsed, err := mail.ParseAddress(a.Email)
	if err != nil || parsed.Address != a.Email || parsed.Name != "" {
		return invalid("%s must be a single plain email address", field)
	}
	return nil
}

func hasLineBreakOrControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMessage, fmt.Sprintf(format, args...))
}

// Class tells a job runner what a failed Send means for retrying.
type Class string

const (
	// ClassTemporary: the message was certainly not accepted and a retry may
	// succeed (network failure, 4xx reply, timeout before DATA completed).
	ClassTemporary Class = "temporary"
	// ClassPermanent: the message was certainly not accepted and retrying the
	// same message with the same configuration will not help (5xx reply,
	// invalid message, authentication or TLS verification failure).
	ClassPermanent Class = "permanent"
	// ClassUnknownOutcome: the server may have accepted the message (the
	// connection failed after the end of DATA was sent). Retrying can send a
	// duplicate; surface the uncertainty instead of reporting success.
	ClassUnknownOutcome Class = "unknownOutcome"
)

// Phase is the step in which delivery failed.
type Phase string

const (
	PhaseValidate Phase = "validate"
	PhaseConfig   Phase = "config"
	PhaseConnect  Phase = "connect" // dial, greeting, STARTTLS, AUTH
	PhaseEnvelope Phase = "envelope"
	PhaseData     Phase = "data"
	PhaseCommit   Phase = "commit" // end of DATA
)

// Error is returned by Send. It never contains the SMTP password.
type Error struct {
	Class Class
	Phase Phase
	Code  int // SMTP reply code when the server sent one, else 0
	Err   error
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("smtp %s failed (%s, reply %d): %v", e.Phase, e.Class, e.Code, e.Err)
	}
	return fmt.Sprintf("smtp %s failed (%s): %v", e.Phase, e.Class, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf returns the retry class of an error returned by a Mailer. Errors
// not produced by this package are treated as an unknown outcome so a
// caller never assumes non-delivery without evidence.
func ClassOf(err error) Class {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	if errors.Is(err, ErrInvalidMessage) {
		return ClassPermanent
	}
	return ClassUnknownOutcome
}
