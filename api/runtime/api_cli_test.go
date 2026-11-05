package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/cli"
)

// cliEnv runs user commands against the real API served over HTTP.
type cliEnv struct {
	t      *testing.T
	url    string
	config string
}

type cliResult struct {
	code     int
	out, err string
}

func (e *cliEnv) run(ctx context.Context, stdin string, args ...string) cliResult {
	e.t.Helper()
	in, err := os.CreateTemp(e.t.TempDir(), "stdin")
	if err != nil {
		e.t.Fatal(err)
	}
	in.WriteString(stdin)
	in.Seek(0, 0)
	defer in.Close()
	var out, errb bytes.Buffer
	args = append(args, "--config", e.config)
	code := Execute(ctx, args, IO{In: in, Out: &out, Err: &errb})
	return cliResult{code: code, out: out.String(), err: errb.String()}
}

func (e *cliEnv) ok(stdin string, args ...string) string {
	e.t.Helper()
	r := e.run(context.Background(), stdin, args...)
	if r.code != cli.ExitOK {
		e.t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.out, r.err)
	}
	return r.out
}

func (e *cliEnv) exit(want int, stdin string, args ...string) cliResult {
	e.t.Helper()
	r := e.run(context.Background(), stdin, args...)
	if r.code != want {
		e.t.Fatalf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", args, r.code, want, r.out, r.err)
	}
	return r
}

func TestCLIAgainstHTTPServer(t *testing.T) {
	f := newTicketFixture(t)
	key := f.createTicket(f.integration, map[string]any{"subject": "CLI ticket"}).Ref()
	srv := httptest.NewServer(f.h)
	t.Cleanup(srv.Close)
	e := &cliEnv{t: t, url: srv.URL, config: filepath.Join(t.TempDir(), "oc", "credentials.json")}

	// Not signed in yet.
	e.exit(cli.ExitAuth, "", "tickets", "list")
	e.exit(cli.ExitUsage, "", "login") // --url is required
	e.exit(cli.ExitUsage, "", "login", "--url", "http://support.example.com", "--login", "dana@example.com", "--password-stdin")
	e.exit(cli.ExitAuth, "wrong password\n", "login", "--url", srv.URL, "--login", "dana@example.com", "--password-stdin")
	// No terminal and no --password-stdin: refuse instead of hanging.
	e.exit(cli.ExitUsage, "", "login", "--url", srv.URL, "--login", "dana@example.com")

	// --login takes a username or an email.
	e.exit(cli.ExitAuth, "wrong password\n", "login", "--url", srv.URL, "--login", "dana", "--password-stdin")
	out := e.ok(agentPassword+"\n", "login", "--url", srv.URL, "--login", "dana", "--password-stdin")
	if !strings.Contains(out, "Signed in") || !strings.Contains(out, "dana@example.com") {
		t.Fatalf("login output: %s", out)
	}
	out = e.ok(agentPassword+"\n", "login", "--url", srv.URL, "--login", "dana@example.com", "--password-stdin")
	if !strings.Contains(out, "Signed in") {
		t.Fatalf("login output: %s", out)
	}
	info, err := os.Stat(e.config)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode: %v %v", info, err)
	}
	if dir, _ := os.Stat(filepath.Dir(e.config)); dir.Mode().Perm() != 0o700 {
		t.Fatalf("credentials dir mode %v", dir.Mode().Perm())
	}

	var listed []map[string]any
	if err := json.Unmarshal([]byte(e.ok("", "tickets", "list", "--json")), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("tickets --json: %v %v", listed, err)
	}
	e.exit(cli.ExitUsage, "", "projects", "list") // removed command
	out = e.ok("", "tickets", "list", "--group", "triage", "--view", "unassigned")
	if !strings.Contains(out, key) || !strings.Contains(out, "CLI ticket") {
		t.Fatalf("tickets list: %s", out)
	}
	e.exit(cli.ExitUsage, "", "tickets", "list", "--group", "No such group")
	e.exit(cli.ExitUsage, "", "tickets", "list", "--project", "MAIL") // removed flag

	out = e.ok("", "tickets", "claim", key)
	if !strings.Contains(out, "Claimed #"+key) || !strings.Contains(out, "Dana Okafor") {
		t.Fatalf("claim: %s", out)
	}
	e.exit(cli.ExitDenied, "", "tickets", "show", "9999")
	e.exit(cli.ExitUsage, "", "tickets", "show", "not-a-key")

	reply := filepath.Join(t.TempDir(), "reply.md")
	os.WriteFile(reply, []byte("We are **looking** into it."), 0o600)
	r := e.exit(cli.ExitUsage, "", "tickets", "reply", key, "--file", reply)
	if !strings.Contains(r.err, "--audience is required") {
		t.Fatalf("missing audience message: %s", r.err)
	}
	e.exit(cli.ExitUsage, "", "tickets", "reply", key, "--file", reply, "--audience", "everyone")
	if out = e.ok("", "tickets", "reply", key, "--file", reply, "--audience", "public"); !strings.Contains(out, "public reply") {
		t.Fatalf("reply: %s", out)
	}
	e.ok("Internal: check the MX records.", "tickets", "reply", key, "--file", "-", "--audience", "agent")
	out = e.ok("", "tickets", "show", key)
	if !strings.Contains(out, "PUBLIC REPLY") || !strings.Contains(out, "AGENT NOTE") || !strings.Contains(out, "check the MX records") {
		t.Fatalf("show: %s", out)
	}

	// An agent cannot read SMTP settings.
	e.exit(cli.ExitDenied, "", "settings", "get", "smtp", "--redacted")

	// The cached access token expires: the CLI refreshes (rotating the
	// refresh token) transparently.
	var creds cli.Credentials
	b, _ := os.ReadFile(e.config)
	json.Unmarshal(b, &creds)
	creds.AccessExpiresAt = time.Now().Add(-time.Minute)
	cli.SaveCredentials(e.config, creds)
	e.ok("", "tickets", "list")
	b, _ = os.ReadFile(e.config)
	var after cli.Credentials
	json.Unmarshal(b, &after)
	if after.RefreshToken == creds.RefreshToken || after.AccessToken == creds.AccessToken {
		t.Fatal("session was not refreshed")
	}

	// Ctrl-C: a cancelled context ends the command with exit code 130.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if r := e.run(cctx, "", "tickets", "list"); r.code != cli.ExitCancelled {
		t.Fatalf("cancelled: exit %d %s", r.code, r.err)
	}

	e.ok("", "logout")
	if _, err := os.Stat(e.config); !os.IsNotExist(err) {
		t.Fatal("credentials file not removed")
	}
	f.refresh(session{Refresh: after.RefreshToken, CSRF: after.CSRFToken}).expect(401)
	e.exit(cli.ExitAuth, "", "tickets", "list")

	// Integration token mode.
	e.exit(cli.ExitAuth, "kase_not_a_token\n", "login", "--url", srv.URL, "--token-stdin")
	e.ok(f.integration+"\n", "login", "--url", srv.URL, "--token-stdin")
	if out = e.ok("", "tickets", "list"); !strings.Contains(out, key) {
		t.Fatalf("integration list: %s", out)
	}
	if out = e.ok("", "logout"); !strings.Contains(out, "stays valid") {
		t.Fatalf("token logout: %s", out)
	}

	// Administrators can read the redacted SMTP settings.
	e.ok(adminPassword+"\n", "login", "--url", srv.URL, "--login", adminEmail, "--password-stdin")
	if out = e.ok("", "settings", "get", "smtp", "--redacted"); !strings.Contains(out, "password") || !strings.Contains(out, "not configured") {
		t.Fatalf("smtp: %s", out)
	}
	e.exit(cli.ExitUsage, "", "settings", "get", "smtp", "--redacted=false")
}

func TestCLILoginWithMFA(t *testing.T) {
	f := newIdentityFixture(t)
	secret, codes := f.enrol(f.admin(), adminPassword)
	srv := httptest.NewServer(f.h)
	t.Cleanup(srv.Close)
	e := &cliEnv{t: t, url: srv.URL, config: filepath.Join(t.TempDir(), "credentials.json")}
	e.exit(cli.ExitAuth, adminPassword+"\n000000\n", "login", "--url", srv.URL, "--login", adminEmail, "--password-stdin")
	// A recovery code (line 2 of standard input) completes the sign-in.
	e.ok(adminPassword+"\n"+codes[1]+"\n", "login", "--url", srv.URL, "--login", adminEmail, "--password-stdin")
	if out := e.ok("", "tickets", "list", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("tickets: %s", out)
	}
	// So does a TOTP code.
	f.clk.Advance(auth.TOTPPeriod)
	e.ok(adminPassword+"\n"+f.code(secret)+"\n", "login", "--url", srv.URL, "--login", adminEmail, "--password-stdin")
	e.ok("", "logout")
}
