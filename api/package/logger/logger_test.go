package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONLoggerRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, Options{Format: FormatJSON, Level: slog.LevelInfo})
	log.Info("login", "userId", "u1", "password", "hunter2", "refreshToken", "abc", "Authorization", "Bearer x")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, buf.String())
	}
	for _, k := range []string{"password", "refreshToken", "Authorization"} {
		if rec[k] != redacted {
			t.Errorf("%s = %v, want redacted", k, rec[k])
		}
	}
	if rec["userId"] != "u1" {
		t.Errorf("userId = %v", rec["userId"])
	}
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatal("password leaked into log output")
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, Options{Format: FormatText, Level: slog.LevelWarn})
	log.Info("hidden")
	log.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	if l, err := ParseLevel("debug"); err != nil || l != slog.LevelDebug {
		t.Fatalf("debug: %v %v", l, err)
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Fatal("expected error for invalid level")
	}
}
