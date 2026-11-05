// Package pagination implements bounded, opaque cursor pagination with a
// stable tie-breaker. Cursors are position markers, never authorization:
// queries must still apply access filters before limits.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"strconv"

	"github.com/arksnorman/open-kase/api/package/fault"
)

const (
	// DefaultLimit applies when a request does not specify a limit.
	DefaultLimit = 25
	// MaxLimit caps every page.
	MaxLimit      = 100
	cursorVersion = 1
)

// Cursor marks the last item of a page: its sort key and unique ID.
type Cursor struct {
	Sort string `json:"s"`
	Id   string `json:"i"`
}

type envelope struct {
	V int    `json:"v"`
	C Cursor `json:"c"`
}

// Encode returns an opaque URL-safe cursor token.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(envelope{V: cursorVersion, C: c})
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode parses a cursor token. An empty token yields a nil cursor (first page).
func Decode(token string) (*Cursor, error) {
	if token == "" {
		return nil, nil
	}
	invalid := fault.Validation(fault.FieldError{Field: "cursor", Code: "invalidCursor", Message: "The page cursor is invalid or expired."})
	if len(token) > 1024 {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, invalid
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.V != cursorVersion || env.C.Id == "" {
		return nil, invalid
	}
	return &env.C, nil
}

// ParseLimit parses a limit query value, applying the default and maximum.
func ParseLimit(s string) (int, error) {
	if s == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > MaxLimit {
		return 0, fault.Validation(fault.FieldError{Field: "limit", Code: "outOfRange",
			Message: "Use a limit from 1 to " + strconv.Itoa(MaxLimit) + ".", Params: map[string]any{"min": 1, "max": MaxLimit}})
	}
	return n, nil
}

// Page is one page of results. Fetch limit+1 rows and pass them to NewPage
// to determine whether a next page exists.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"` // null on the final page
}

// NewPage trims rows to limit and builds the next cursor from the last
// returned item when more rows exist.
func NewPage[T any](rows []T, limit int, cursorOf func(T) Cursor) Page[T] {
	if rows == nil {
		rows = []T{}
	}
	if len(rows) <= limit {
		return Page[T]{Items: rows}
	}
	items := rows[:limit]
	next := cursorOf(items[len(items)-1]).Encode()
	return Page[T]{Items: items, NextCursor: &next}
}
