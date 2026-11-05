// Package fault defines transport-independent application errors and their
// RFC 9457 problem-details representation.
package fault

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/arksnorman/open-kase/api/package/product"
)

// Kind classifies an error and determines its HTTP status.
type Kind string

const (
	KindMalformed        Kind = "malformed"        // 400
	KindUnauthenticated  Kind = "unauthenticated"  // 401
	KindForbidden        Kind = "forbidden"        // 403
	KindNotFound         Kind = "notFound"         // 404
	KindMethodNotAllowed Kind = "methodNotAllowed" // 405
	KindConflict         Kind = "conflict"         // 409
	KindGone             Kind = "gone"             // 410
	KindPrecondition     Kind = "precondition"     // 428
	KindTooLarge         Kind = "tooLarge"         // 413
	KindUnsupportedMedia Kind = "unsupportedMedia" // 415
	KindValidation       Kind = "validation"       // 422
	KindRateLimited      Kind = "rateLimited"      // 429
	KindUnavailable      Kind = "unavailable"      // 503
	KindInternal         Kind = "internal"         // 500
)

var kindStatus = map[Kind]int{
	KindMalformed:        http.StatusBadRequest,
	KindUnauthenticated:  http.StatusUnauthorized,
	KindForbidden:        http.StatusForbidden,
	KindNotFound:         http.StatusNotFound,
	KindMethodNotAllowed: http.StatusMethodNotAllowed,
	KindConflict:         http.StatusConflict,
	KindGone:             http.StatusGone,
	KindPrecondition:     http.StatusPreconditionRequired,
	KindTooLarge:         http.StatusRequestEntityTooLarge,
	KindUnsupportedMedia: http.StatusUnsupportedMediaType,
	KindValidation:       http.StatusUnprocessableEntity,
	KindRateLimited:      http.StatusTooManyRequests,
	KindUnavailable:      http.StatusServiceUnavailable,
	KindInternal:         http.StatusInternalServerError,
}

var kindTitle = map[Kind]string{
	KindMalformed:        "The request could not be read",
	KindUnauthenticated:  "Authentication is required",
	KindForbidden:        "This action is not permitted",
	KindNotFound:         "The resource was not found",
	KindMethodNotAllowed: "The method is not allowed",
	KindConflict:         "The request conflicts with the current state",
	KindGone:             "The resource is no longer available",
	KindPrecondition:     "A precondition header is required",
	KindTooLarge:         "The request is too large",
	KindUnsupportedMedia: "The content type is not supported",
	KindValidation:       "Some fields are invalid",
	KindRateLimited:      "Too many requests",
	KindUnavailable:      "The service is temporarily unavailable",
	KindInternal:         "An unexpected error occurred",
}

// FieldError describes one invalid input field with a stable code.
type FieldError struct {
	Field   string         `json:"field"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Params  map[string]any `json:"params,omitempty"`
}

// Error is an application error safe to expose to clients. Cause is kept for
// diagnostics only and is never serialized.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Fields  []FieldError
	Cause   error
	// RetryAfter is set on rate-limited errors; transports may emit it as
	// a Retry-After header.
	RetryAfter time.Duration
	// Actions are follow-up actions the client may offer (e.g. "Create
	// follow-up ticket" for ticketArchived).
	Actions []Action
}

// Action is a suggested next step for a problem: a stable rel, a short
// label and the REST request that performs it.
type Action struct {
	Rel    string `json:"rel"`
	Title  string `json:"title"`
	Method string `json:"method"`
	Href   string `json:"href"`
}

// WithActions returns a copy of e carrying actions.
func (e *Error) WithActions(actions ...Action) *Error {
	c := *e
	c.Actions = append([]Action(nil), actions...)
	return &c
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Code, e.Cause)
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s: %s", e.Kind, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

// New creates an error of the given kind and stable code.
func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Wrap creates an error that retains cause for logs.
func Wrap(kind Kind, code string, cause error) *Error {
	return &Error{Kind: kind, Code: code, Cause: cause}
}

// Validation returns a 422 error carrying field errors.
func Validation(fields ...FieldError) *Error {
	return &Error{Kind: KindValidation, Code: "validationFailed", Fields: fields}
}

// NotFound returns a 404 error with a stable code.
func NotFound(code string) *Error { return New(KindNotFound, code, "") }

// Conflict returns a 409 error with a stable code.
func Conflict(code, message string) *Error { return New(KindConflict, code, message) }

// Forbidden returns a 403 error with a stable code.
func Forbidden(code, message string) *Error { return New(KindForbidden, code, message) }

// Unauthenticated returns a 401 error with a stable code.
func Unauthenticated(code, message string) *Error { return New(KindUnauthenticated, code, message) }

// Field returns a 422 validation error for a single field.
func Field(field, code, message string) *Error {
	return Validation(FieldError{Field: field, Code: code, Message: message})
}

// RateLimited returns a 429 error that tells the client when to retry.
func RateLimited(code, message string, retryAfter time.Duration) *Error {
	return &Error{Kind: KindRateLimited, Code: code, Message: message, RetryAfter: retryAfter}
}

// Internal wraps an unexpected error.
func Internal(cause error) *Error { return Wrap(KindInternal, "internalError", cause) }

// As extracts an *Error from err; unknown errors become internal errors.
func As(err error) *Error {
	var fe *Error
	if errors.As(err, &fe) {
		return fe
	}
	return Internal(err)
}

// Status returns the HTTP status for a kind.
func (k Kind) Status() int {
	if s, ok := kindStatus[k]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Problem is the problem-details response body.
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Code      string       `json:"code"`
	Detail    string       `json:"detail,omitempty"`
	RequestId string       `json:"requestId,omitempty"`
	Errors    []FieldError `json:"errors,omitempty"`
	// Actions suggest follow-up requests (e.g. createFollowUp for
	// ticketArchived).
	Actions []Action `json:"actions,omitempty"`
}

// ToProblem converts any error into a client-safe problem. Internal error
// causes are never exposed.
func ToProblem(err error, requestID string) Problem {
	fe := As(err)
	title, ok := kindTitle[fe.Kind]
	if !ok {
		title = kindTitle[KindInternal]
	}
	p := Problem{
		Type:      product.ErrorTypeURN(string(fe.Kind)),
		Title:     title,
		Status:    fe.Kind.Status(),
		Code:      fe.Code,
		RequestId: requestID,
		Errors:    fe.Fields,
		Actions:   fe.Actions,
	}
	if fe.Kind != KindInternal {
		p.Detail = fe.Message
	}
	return p
}
