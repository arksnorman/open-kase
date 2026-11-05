// Package logger builds the single structured log/slog logger that is
// injected throughout the application.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Format selects the log encoding.
type Format string

const (
	// FormatJSON emits one JSON object per line (production default).
	FormatJSON Format = "json"
	// FormatText emits human-readable key=value lines (development).
	FormatText Format = "text"
)

// Options configures a logger.
type Options struct {
	Format Format
	Level  slog.Level
}

// New returns a logger writing to w. Attributes whose keys look like
// credentials are redacted regardless of the caller.
func New(w io.Writer, opts Options) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{Level: opts.Level, ReplaceAttr: redact}
	var h slog.Handler
	if opts.Format == FormatText {
		h = slog.NewTextHandler(w, handlerOpts)
	} else {
		h = slog.NewJSONHandler(w, handlerOpts)
	}
	return slog.New(h)
}

// ParseLevel converts debug/info/warn/error into a slog level.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return slog.LevelInfo, fmt.Errorf("invalid log level %q", s)
	}
	return l, nil
}

// Discard returns a logger that drops all records (useful in tests).
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

const redacted = "[REDACTED]"

var sensitiveKeys = []string{"password", "secret", "token", "authorization", "cookie", "credential", "api_key", "apiKey", "apikey", "master_key", "masterKey", "private_key", "privateKey"}

func redact(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(a.Key)
	for _, s := range sensitiveKeys {
		if strings.Contains(key, s) {
			return slog.String(a.Key, redacted)
		}
	}
	return a
}
