package validation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/arksnorman/open-kase/api/package/fault"
)

// DefaultMaxBodyBytes bounds JSON command bodies unless a route sets its own.
const DefaultMaxBodyBytes = 1 << 20

// DecodeStrict decodes exactly one JSON object from r into dst, rejecting
// unknown fields, MongoDB operator-like keys, trailing data and bodies larger
// than maxBytes.
func DecodeStrict(r io.Reader, maxBytes int64, dst any) error {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fault.New(fault.KindTooLarge, "bodyTooLarge", "The request body is too large.")
		}
		return fault.Wrap(fault.KindMalformed, "unreadableBody", err)
	}
	if int64(len(raw)) > maxBytes {
		return fault.New(fault.KindTooLarge, "bodyTooLarge", "The request body is too large.")
	}
	if key, bad := findOperatorKey(raw); bad {
		return fault.Validation(fault.FieldError{Field: key, Code: CodeUnknownField, Message: "This field is not accepted."})
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if dec.More() {
		return fault.New(fault.KindMalformed, "malformedJson", "The body must contain a single JSON object.")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fault.New(fault.KindMalformed, "malformedJson", "The body must contain a single JSON object.")
	}
	return nil
}

func decodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "$"
		}
		return fault.Validation(fault.FieldError{Field: field, Code: CodeInvalidType,
			Message: fmt.Sprintf("Expected a %s value.", typeErr.Type.Kind())})
	}
	if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
		field := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		return fault.Validation(fault.FieldError{Field: field, Code: CodeUnknownField, Message: "This field is not accepted."})
	}
	return fault.Wrap(fault.KindMalformed, "malformedJson", err)
}

// findOperatorKey reports the first object key that starts with '$' or
// contains a '.', which are never valid API field names and would be unsafe
// if a payload ever reached a MongoDB query.
func findOperatorKey(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Track whether the next string token in an object is a key.
	type frame struct{ object, expectKey bool }
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		top := len(stack) - 1
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				if top >= 0 && stack[top].object {
					stack[top].expectKey = true
				}
				stack = append(stack, frame{object: true, expectKey: true})
				continue
			case '[':
				if top >= 0 && stack[top].object {
					stack[top].expectKey = true
				}
				stack = append(stack, frame{})
				continue
			default:
				stack = stack[:top]
				continue
			}
		case string:
			if top >= 0 && stack[top].object && stack[top].expectKey {
				if strings.HasPrefix(t, "$") || strings.Contains(t, ".") {
					return t, true
				}
				stack[top].expectKey = false
				continue
			}
		}
		if top >= 0 && stack[top].object {
			stack[top].expectKey = true
		}
	}
}
