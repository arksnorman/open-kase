package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://support.example.com/": "https://support.example.com",
		"http://127.0.0.1:8080":        "http://127.0.0.1:8080",
		"http://localhost:8080/oc/":    "http://localhost:8080/oc",
	} {
		if got, err := normalizeURL(in, false); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"support.example.com", "ftp://x", "https://u:p@x", "http://10.0.0.5:8080"} {
		if _, err := normalizeURL(in, false); exitCode(context.Background(), err) != ExitUsage {
			t.Errorf("%s accepted", in)
		}
	}
	if _, err := normalizeURL("http://10.0.0.5:8080", true); err != nil {
		t.Errorf("--allow-http: %v", err)
	}
}

func TestCredentialsFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d", "credentials.json")
	if _, err := LoadCredentials(path); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("missing file: %v", err)
	}
	if err := SaveCredentials(path, Credentials{URL: "https://x", Kind: KindToken, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadCredentials(path); err != nil || c.Token != "t" {
		t.Fatalf("load: %+v %v", c, err)
	}
	os.Chmod(path, 0o644)
	if _, err := LoadCredentials(path); err == nil {
		t.Fatal("world-readable credentials accepted")
	}
}

func TestExitCodes(t *testing.T) {
	ctx := context.Background()
	for status, want := range map[int]int{401: ExitAuth, 403: ExitDenied, 404: ExitDenied, 409: ExitConflict, 422: ExitConflict, 429: ExitThrottled, 500: ExitFailure} {
		if got := exitCode(ctx, &APIError{Status: status}); got != want {
			t.Errorf("%d: %d want %d", status, got, want)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if exitCode(cctx, errors.New("x")) != ExitCancelled {
		t.Error("cancel")
	}
}
