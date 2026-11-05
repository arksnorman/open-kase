// Package validation is the single source of truth for API field rules,
// command schemas and stable validation error codes. Rules are contextual
// (ticket.subject, project.name) rather than generic (name).
//
// REST, MCP, CLI and workers all validate through this package. It checks
// shape only; authorization, uniqueness and state transitions belong to
// application services.
package validation

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/arksnorman/open-kase/api/package/fault"
)

// Stable field error codes. Clients may localize on these.
const (
	CodeRequired      = "required"
	CodeTooShort      = "tooShort"
	CodeTooLong       = "tooLong"
	CodeOutOfRange    = "outOfRange"
	CodeInvalidFormat = "invalidFormat"
	CodeInvalidChoice = "invalidChoice"
	CodeInvalidChars  = "invalidCharacters"
	CodeUnknownField  = "unknownField"
	CodeInvalidType   = "invalidType"
	CodeMismatch      = "mismatch"
)

// Kind is the value shape a rule validates.
type Kind string

const (
	KindText   Kind = "text"
	KindInt    Kind = "integer"
	KindEnum   Kind = "enum"
	KindEmail  Kind = "email"
	KindURL    Kind = "url"
	KindHost   Kind = "hostname"
	KindSecret Kind = "secret"
)

// Rule describes one contextual field.
type Rule struct {
	Field     string // catalog identifier, e.g. "ticket.subject"
	Kind      Kind
	Required  bool
	MinLen    int // Unicode code points
	MaxLen    int // Unicode code points
	Min, Max  int64
	Pattern   *regexp.Regexp
	Choices   []string
	Multiline bool   // allow \n and \t in text
	Message   string // user-facing guidance used for format/range errors
}

var catalog = map[string]Rule{}

func define(r Rule) Rule {
	if _, dup := catalog[r.Field]; dup {
		panic("validation: duplicate rule " + r.Field)
	}
	catalog[r.Field] = r
	return r
}

// Lookup returns a catalog rule by identifier.
func Lookup(field string) (Rule, bool) {
	r, ok := catalog[field]
	return r, ok
}

// All returns every catalog rule sorted by identifier, for schema export.
func All() []Rule {
	out := make([]Rule, 0, len(catalog))
	for _, r := range catalog {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Rule) int { return strings.Compare(a.Field, b.Field) })
	return out
}

// CheckString validates a string value. An empty value passes when the rule
// is optional.
func (r Rule) CheckString(field, v string) *fault.FieldError {
	if v == "" {
		if r.Required {
			return fe(field, CodeRequired, "This field is required.", nil)
		}
		return nil
	}
	if !utf8.ValidString(v) {
		return fe(field, CodeInvalidChars, "Use valid UTF-8 text.", nil)
	}
	// A required text value must contain more than white space (a blank
	// name would otherwise reach storage, whose schema rejects it).
	if r.Required && r.Kind == KindText && strings.TrimSpace(v) == "" {
		return fe(field, CodeRequired, "This field is required.", nil)
	}
	for _, c := range v {
		if unicode.IsControl(c) && !(r.Multiline && (c == '\n' || c == '\t' || c == '\r')) {
			return fe(field, CodeInvalidChars, "Remove control characters.", nil)
		}
	}
	n := utf8.RuneCountInString(v)
	if r.MinLen > 0 && n < r.MinLen {
		return fe(field, CodeTooShort, r.msg(fmt.Sprintf("Use at least %d characters.", r.MinLen)), map[string]any{"min": r.MinLen})
	}
	if r.MaxLen > 0 && n > r.MaxLen {
		return fe(field, CodeTooLong, r.msg(fmt.Sprintf("Use at most %d characters.", r.MaxLen)), map[string]any{"max": r.MaxLen})
	}
	switch r.Kind {
	case KindEnum:
		if !slices.Contains(r.Choices, v) {
			return fe(field, CodeInvalidChoice, r.msg("Choose one of the allowed values."), map[string]any{"choices": r.Choices})
		}
	case KindEmail:
		addr, err := mail.ParseAddress(v)
		if err != nil || addr.Address != v || !strings.Contains(v[strings.LastIndexByte(v, '@')+1:], ".") {
			return fe(field, CodeInvalidFormat, r.msg("Enter a valid email address."), nil)
		}
	case KindURL:
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fe(field, CodeInvalidFormat, r.msg("Enter an http or https URL."), nil)
		}
	case KindHost:
		if !hostPattern.MatchString(v) {
			return fe(field, CodeInvalidFormat, r.msg("Enter a valid host name."), nil)
		}
	}
	if r.Pattern != nil && !r.Pattern.MatchString(v) {
		return fe(field, CodeInvalidFormat, r.msg("The value has an invalid format."), nil)
	}
	return nil
}

// CheckInt validates an integer value against the rule's range.
func (r Rule) CheckInt(field string, v int64) *fault.FieldError {
	if v < r.Min || v > r.Max {
		return fe(field, CodeOutOfRange, r.msg(fmt.Sprintf("Use a value from %d to %d.", r.Min, r.Max)), map[string]any{"min": r.Min, "max": r.Max})
	}
	return nil
}

func (r Rule) msg(fallback string) string {
	if r.Message != "" {
		return r.Message
	}
	return fallback
}

var hostPattern = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func fe(field, code, message string, params map[string]any) *fault.FieldError {
	return &fault.FieldError{Field: field, Code: code, Message: message, Params: params}
}

// Collector accumulates field errors for one command.
type Collector struct {
	errs []fault.FieldError
}

// String validates a string field using a catalog rule.
func (c *Collector) String(r Rule, field, v string) {
	c.add(r.CheckString(field, v))
}

// Int validates an integer field using a catalog rule.
func (c *Collector) Int(r Rule, field string, v int64) {
	c.add(r.CheckInt(field, v))
}

// Add records a custom field error (e.g. password confirmation mismatch).
func (c *Collector) Add(field, code, message string) {
	c.errs = append(c.errs, fault.FieldError{Field: field, Code: code, Message: message})
}

func (c *Collector) add(e *fault.FieldError) {
	if e != nil {
		c.errs = append(c.errs, *e)
	}
}

// Err returns a validation fault, or nil when no errors were collected.
func (c *Collector) Err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return fault.Validation(c.errs...)
}

// ValidID reports whether s has the opaque identifier format used at the
// API boundary. Callers treat an invalid ID like an unknown one (404).
func ValidID(s string) bool { return ObjectID.CheckString("id", s) == nil }

// OptionalString validates v only when it is non-nil (a PATCH field that
// was sent). An empty value is rejected when the rule requires a value.
func (c *Collector) OptionalString(r Rule, field string, v *string) {
	if v == nil {
		return
	}
	if *v == "" && (r.Required || r.MinLen > 0) {
		c.Add(field, CodeRequired, "This field cannot be empty.")
		return
	}
	c.String(r, field, *v)
}
