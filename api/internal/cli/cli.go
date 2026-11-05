package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/arksnorman/open-kase/api/package/product"
)

// IO carries the process streams. In may be nil (no input available).
type IO struct {
	In  *os.File
	Out io.Writer
	Err io.Writer
}

// Commands lists the user commands with their one-line summaries, for the
// binary's help output.
func Commands() [][2]string {
	return [][2]string{
		{"login", "Sign in to a server: 'login --url URL' (password prompt) or '--token-stdin' for an API token"},
		{"logout", "Sign out and delete the stored credentials"},
		{"tickets", "Work tickets: 'tickets list|show|claim|reply'"},
		{"settings", "Read installation settings: 'settings get smtp --redacted'"},
	}
}

// Run executes a user command and returns its exit code.
func Run(ctx context.Context, name string, args []string, stdio IO) int {
	r := &runner{io: stdio, now: func() time.Time { return time.Now().UTC() }}
	if stdio.In != nil {
		r.in = bufio.NewReader(stdio.In)
	}
	var err error
	switch name {
	case "login":
		err = r.login(ctx, args)
	case "logout":
		err = r.logout(ctx, args)
	case "tickets":
		err = r.tickets(ctx, args)
	case "settings":
		err = r.settings(ctx, args)
	default:
		err = usagef("unknown command %q", name)
	}
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	code := exitCode(ctx, err)
	if err != nil {
		if code == ExitCancelled {
			fmt.Fprintln(stdio.Err, "cancelled")
		} else {
			fmt.Fprintf(stdio.Err, "%s: %v\n", product.Binary, err)
		}
	}
	return code
}

type runner struct {
	io  IO
	in  *bufio.Reader
	now func() time.Time
}

// common flags shared by every user command.
type common struct {
	config string
	json   bool
}

func (r *runner) flags(name, usage string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(product.Binary+" "+name, flag.ContinueOnError)
	fs.SetOutput(r.io.Err)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s %s\n\nFlags:\n", product.Binary, usage)
		fs.PrintDefaults()
	}
	c := &common{}
	fs.StringVar(&c.config, "config", "", "credentials file (default $"+ConfigEnv+" or the user config directory)")
	fs.BoolVar(&c.json, "json", false, "print JSON")
	return fs, c
}

// parse parses flags; positional arguments may precede or follow flags.
func parse(fs *flag.FlagSet, args []string, positional int) ([]string, error) {
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{msg: err.Error()}
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		rest = rest[1:]
	}
	if len(pos) != positional {
		fs.Usage()
		return nil, usagef("expected %d argument(s), got %d", positional, len(pos))
	}
	return pos, nil
}

func (c *common) path() (string, error) {
	if c.config != "" {
		return c.config, nil
	}
	return DefaultConfigPath()
}

// client loads the stored credentials.
func (r *runner) client(c *common) (*client, error) {
	path, err := c.path()
	if err != nil {
		return nil, err
	}
	creds, err := LoadCredentials(path)
	if err != nil {
		return nil, err
	}
	return &client{http: newHTTPClient(), creds: creds, path: path, now: r.now}, nil
}

func (r *runner) printJSON(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		_, err = r.io.Out.Write(raw)
		return err
	}
	enc := json.NewEncoder(r.io.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (r *runner) table() *tabwriter.Writer { return tabwriter.NewWriter(r.io.Out, 0, 4, 2, ' ', 0) }

// readLine reads one line of standard input (for --password-stdin and
// friends), without the line ending.
func (r *runner) readLine(what string) (string, error) {
	if r.in == nil {
		return "", usagef("no standard input to read %s from", what)
	}
	line, err := r.in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", usagef("could not read %s from standard input", what)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (r *runner) interactive() bool { return r.io.In != nil && term.IsTerminal(int(r.io.In.Fd())) }

// prompt asks for a value on the terminal; hidden values are not echoed.
func (r *runner) prompt(label string, hidden bool) (string, error) {
	if !r.interactive() {
		return "", usagef("%s is required; standard input is not a terminal (see --help for non-interactive flags)", strings.ToLower(label))
	}
	fmt.Fprintf(r.io.Err, "%s: ", label)
	if hidden {
		b, err := term.ReadPassword(int(r.io.In.Fd()))
		fmt.Fprintln(r.io.Err)
		return string(b), err
	}
	return r.readLine(strings.ToLower(label))
}

// ---- login / logout ----

func (r *runner) login(ctx context.Context, args []string) error {
	fs, c := r.flags("login", "login --url URL [--login EMAIL_OR_USERNAME] [--password-stdin] | --token-stdin")
	base := fs.String("url", "", "server base URL, e.g. https://support.example.com (required)")
	login := fs.String("login", "", "sign-in email or username (prompted when omitted)")
	pwStdin := fs.Bool("password-stdin", false, "read the password (and then an MFA code, if asked) from standard input lines")
	tokStdin := fs.Bool("token-stdin", false, "read an API token from standard input instead of signing in as a user")
	allowHTTP := fs.Bool("allow-http", false, "allow plain HTTP to a non-loopback server")
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	if *base == "" {
		return usagef("--url is required")
	}
	u, err := normalizeURL(*base, *allowHTTP)
	if err != nil {
		return err
	}
	path, err := c.path()
	if err != nil {
		return err
	}
	cl := &client{http: newHTTPClient(), creds: Credentials{URL: u}, readOnly: true, now: r.now}
	var creds Credentials
	if *tokStdin {
		creds, err = r.loginToken(ctx, cl)
	} else {
		creds, err = r.loginPassword(ctx, cl, *login, *pwStdin)
	}
	if err != nil {
		return err
	}
	if err := SaveCredentials(path, creds); err != nil {
		return err
	}
	if c.json {
		return r.printJSON(mustJSON(map[string]any{"url": creds.URL, "kind": creds.Kind, "email": creds.Email, "credentialsFile": path}))
	}
	who := "an API token"
	if creds.Email != "" {
		who = creds.Email
	}
	fmt.Fprintf(r.io.Out, "Signed in to %s as %s (credentials in %s)\n", creds.URL, who, path)
	return nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (r *runner) loginToken(ctx context.Context, cl *client) (Credentials, error) {
	tok, err := r.readLine("the token")
	if err != nil {
		return Credentials{}, err
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return Credentials{}, usagef("the token on standard input is empty")
	}
	cl.creds.Kind, cl.creds.Token = KindToken, tok
	// Verify the token before storing it.
	if _, err := cl.json(ctx, request{method: http.MethodGet, path: "/project"}, nil); err != nil {
		var pe *APIError
		if errors.As(err, &pe) && pe.Status == http.StatusUnauthorized {
			return Credentials{}, authError{msg: "the server rejected the token"}
		}
		return Credentials{}, err
	}
	return cl.creds, nil
}

type loginResponse struct {
	MFARequired          bool    `json:"mfaRequired"`
	MFAToken             *string `json:"mfaToken"`
	AccessToken          *string `json:"accessToken"`
	ExpiresIn            *int    `json:"expiresIn"`
	MFAEnrolmentRequired bool    `json:"mfaEnrolmentRequired"`
	User                 *struct {
		Email string `json:"email"`
	} `json:"user"`
}

func (r *runner) loginPassword(ctx context.Context, cl *client, login string, pwStdin bool) (Credentials, error) {
	var err error
	if login == "" {
		if login, err = r.prompt("Email or username", false); err != nil {
			return Credentials{}, err
		}
	}
	var password string
	if pwStdin {
		password, err = r.readLine("the password")
	} else {
		password, err = r.prompt("Password", true)
	}
	if err != nil {
		return Credentials{}, err
	}
	hdr := map[string]string{"Origin": origin(cl.creds.URL)}
	resp, err := cl.raw(ctx, request{method: http.MethodPost, path: "/auth/login", headers: hdr,
		body: map[string]string{"login": strings.TrimSpace(login), "password": password}}, "")
	if err != nil {
		return Credentials{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, decodeError(resp)
	}
	var lr loginResponse
	err = json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if err != nil {
		return Credentials{}, err
	}
	if lr.MFARequired && lr.MFAToken != nil {
		var code string
		if pwStdin {
			code, err = r.readLine("the MFA code")
		} else {
			code, err = r.prompt("Authentication code (or recovery code)", false)
		}
		if err != nil {
			return Credentials{}, err
		}
		code = strings.TrimSpace(code)
		body := map[string]string{"mfaToken": *lr.MFAToken}
		if len(code) == 6 && strings.Trim(code, "0123456789") == "" {
			body["code"] = code
		} else {
			body["recoveryCode"] = code
		}
		if resp, err = cl.raw(ctx, request{method: http.MethodPost, path: "/auth/mfa", headers: hdr, body: body}, ""); err != nil {
			return Credentials{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return Credentials{}, decodeError(resp)
		}
		lr = loginResponse{}
		err = json.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close()
		if err != nil {
			return Credentials{}, err
		}
	}
	creds := Credentials{URL: cl.creds.URL, Kind: KindSession}
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case "kase_refresh":
			creds.RefreshToken = ck.Value
		case "kase_csrf":
			creds.CSRFToken = ck.Value
		}
	}
	if lr.AccessToken == nil || creds.RefreshToken == "" || creds.CSRFToken == "" {
		return Credentials{}, errors.New("the server did not return a session")
	}
	creds.AccessToken = *lr.AccessToken
	if lr.ExpiresIn != nil {
		creds.AccessExpiresAt = r.now().Add(time.Duration(*lr.ExpiresIn) * time.Second)
	}
	if lr.User != nil {
		creds.Email = lr.User.Email
	}
	if lr.MFAEnrolmentRequired {
		fmt.Fprintln(r.io.Err, "warning: this installation requires administrators to enrol MFA; enrol in the web app before using other commands")
	}
	return creds, nil
}

func (r *runner) logout(ctx context.Context, args []string) error {
	fs, c := r.flags("logout", "logout")
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	path, err := c.path()
	if err != nil {
		return err
	}
	creds, err := LoadCredentials(path)
	if errors.Is(err, ErrNotLoggedIn) {
		fmt.Fprintln(r.io.Out, "Not signed in.")
		return nil
	}
	if err != nil {
		return err
	}
	var remoteErr error
	if creds.Kind == KindSession {
		cl := &client{http: newHTTPClient(), creds: creds, now: r.now}
		resp, err := cl.raw(ctx, request{method: http.MethodPost, path: "/auth/logout", headers: map[string]string{
			"Cookie":       "kase_refresh=" + creds.RefreshToken + "; kase_csrf=" + creds.CSRFToken,
			"X-CSRF-Token": creds.CSRFToken, "Origin": origin(creds.URL)}}, "")
		switch {
		case err != nil:
			remoteErr = err
		case resp.StatusCode != http.StatusNoContent:
			remoteErr = decodeError(resp)
		default:
			resp.Body.Close()
		}
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if remoteErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(r.io.Err, "warning: the server session may still be active: %v\n", remoteErr)
	}
	if creds.Kind == KindToken {
		fmt.Fprintln(r.io.Out, "Removed the stored API token. It stays valid until an administrator revokes it.")
		return nil
	}
	fmt.Fprintln(r.io.Out, "Signed out.")
	return nil
}

// ---- paging ----

type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// all follows cursors until max items are collected.
func all[T any](ctx context.Context, cl *client, path string, q url.Values, max int) ([]T, error) {
	out := []T{}
	if q == nil {
		q = url.Values{}
	}
	for {
		q.Set("limit", strconv.Itoa(min(100, max-len(out))))
		var p page[T]
		if _, err := cl.json(ctx, request{method: http.MethodGet, path: path, query: q}, &p); err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == nil || len(out) >= max {
			return out, nil
		}
		q.Set("cursor", *p.NextCursor)
	}
}

// ---- settings ----

func (r *runner) settings(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "get" || args[1] != "smtp" {
		return usagef("usage: %s settings get smtp --redacted [--json]", product.Binary)
	}
	fs, c := r.flags("settings get smtp", "settings get smtp --redacted [--json]")
	redacted := fs.Bool("redacted", true, "show secrets as configured/not configured (the API never returns them)")
	if _, err := parse(fs, args[2:], 0); err != nil {
		return err
	}
	if !*redacted {
		return usagef("SMTP secrets are never returned by the API; only --redacted output is available")
	}
	cl, err := r.client(c)
	if err != nil {
		return err
	}
	var s struct {
		Enabled            bool   `json:"enabled"`
		Host               string `json:"host"`
		Port               int    `json:"port"`
		Username           string `json:"username"`
		PasswordConfigured bool   `json:"passwordConfigured"`
		FromAddress        string `json:"fromAddress"`
		FromName           string `json:"fromName"`
		TLSMode            string `json:"tlsMode"`
		CACertificate      string `json:"caCertificate"`
		TLSSkipVerify      bool   `json:"tlsSkipVerify"`
	}
	raw, err := cl.json(ctx, request{method: http.MethodGet, path: "/settings/smtp"}, &s)
	if err != nil {
		return err
	}
	if c.json {
		return r.printJSON(raw)
	}
	pw := "not configured"
	if s.PasswordConfigured {
		pw = "configured (redacted)"
	}
	tw := r.table()
	for _, kv := range [][2]string{{"enabled", strconv.FormatBool(s.Enabled)}, {"host", s.Host}, {"port", strconv.Itoa(s.Port)},
		{"username", s.Username}, {"password", pw}, {"fromAddress", s.FromAddress}, {"fromName", s.FromName}, {"tlsMode", s.TLSMode},
		{"caCertificate", map[bool]string{true: "configured", false: "none"}[s.CACertificate != ""]},
		{"tlsSkipVerify", strconv.FormatBool(s.TLSSkipVerify)}} {
		fmt.Fprintf(tw, "%s\t%s\n", kv[0], kv[1])
	}
	return tw.Flush()
}
