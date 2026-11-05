package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/package/product"
)

// Exit codes of the user commands (docs/cli.md).
const (
	ExitOK        = 0
	ExitFailure   = 1   // server, network or unexpected failure
	ExitUsage     = 2   // invalid arguments or missing input
	ExitAuth      = 3   // not signed in, session ended or credentials rejected
	ExitDenied    = 4   // forbidden or not found
	ExitConflict  = 5   // validation, conflict or precondition failure
	ExitThrottled = 6   // rate limited
	ExitCancelled = 130 // interrupted (Ctrl-C)
)

// APIError is a problem+json error returned by the server.
type APIError struct {
	Status int
	Code   string
	Detail string
	Fields []struct {
		Field   string `json:"field"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	RetryAfter string
}

func (e *APIError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, HTTP %d)", msg, e.Code, e.Status)
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "\n  %s: %s", f.Field, f.Message)
	}
	if e.RetryAfter != "" {
		fmt.Fprintf(&b, "\n  retry after %s seconds", e.RetryAfter)
	}
	return b.String()
}

// usageError is a problem with the command line or local input.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{msg: fmt.Sprintf(format, a...)} }

// authError means the stored credentials cannot be used.
type authError struct{ msg string }

func (e authError) Error() string { return e.msg }

// exitCode maps an error to the documented exit code.
func exitCode(ctx context.Context, err error) int {
	if err == nil {
		return ExitOK
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return ExitCancelled
	}
	var ue usageError
	if errors.As(err, &ue) {
		return ExitUsage
	}
	var ae authError
	if errors.As(err, &ae) || errors.Is(err, ErrNotLoggedIn) {
		return ExitAuth
	}
	var pe *APIError
	if errors.As(err, &pe) {
		switch {
		case pe.Status == http.StatusUnauthorized:
			return ExitAuth
		case pe.Status == http.StatusForbidden || pe.Status == http.StatusNotFound:
			return ExitDenied
		case pe.Status == http.StatusTooManyRequests:
			return ExitThrottled
		case pe.Status == http.StatusConflict || pe.Status == http.StatusUnprocessableEntity ||
			pe.Status == http.StatusPreconditionFailed || pe.Status == http.StatusPreconditionRequired ||
			pe.Status == http.StatusBadRequest || pe.Status == http.StatusGone:
			return ExitConflict
		}
	}
	return ExitFailure
}

// normalizeURL validates the server base URL. Plain HTTP is accepted only
// for loopback hosts unless allowHTTP is set (credentials would cross the
// network unencrypted).
func normalizeURL(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", usagef("--url must be an absolute http(s) URL such as https://support.example.com")
	}
	if u.Scheme == "http" && !allowHTTP && !isLoopback(u.Hostname()) {
		return "", usagef("refusing plain HTTP to %s: credentials would be sent unencrypted (use https, or --allow-http on a trusted network)", u.Host)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// client calls the REST API with the stored credentials.
type client struct {
	http     *http.Client
	creds    Credentials
	path     string // credentials file, rewritten after refresh rotation
	readOnly bool   // credentials not persisted (login verification)
	now      func() time.Time
}

func newHTTPClient() *http.Client {
	// No overall timeout: downloads may stream for a while. Commands pass a
	// cancellable context (Ctrl-C) and the transport bounds connection setup.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 2 * time.Minute
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // never forward credentials to another location
	}}
}

// origin is the Origin header value for cookie-authenticated auth routes.
func origin(base string) string {
	u, _ := url.Parse(base)
	return u.Scheme + "://" + u.Host
}

type request struct {
	method  string
	path    string // under /api/v1
	query   url.Values
	body    any
	headers map[string]string
	noAuth  bool
}

// raw performs one request without credential handling.
func (c *client) raw(ctx context.Context, r request, bearer string) (*http.Response, error) {
	u := c.creds.URL + "/api/v1" + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", product.Binary+"-cli/"+product.Version)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return c.http.Do(req)
}

// decodeError turns a non-2xx response into an *APIError.
func decodeError(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{Status: resp.StatusCode, Code: "httpError", RetryAfter: resp.Header.Get("Retry-After")}
	var p struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Title  string `json:"title"`
		Errors []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &p) == nil && p.Code != "" {
		e.Code, e.Detail, e.Fields = p.Code, p.Detail, p.Errors
		if e.Detail == "" {
			e.Detail = p.Title
		}
	}
	return e
}

// bearer returns a usable access credential, refreshing the session when
// the cached access token is about to expire.
func (c *client) bearer(ctx context.Context) (string, error) {
	if c.creds.Kind == KindToken {
		return c.creds.Token, nil
	}
	if c.creds.AccessToken != "" && c.now().Add(30*time.Second).Before(c.creds.AccessExpiresAt) {
		return c.creds.AccessToken, nil
	}
	return c.refresh(ctx)
}

// refresh rotates the refresh token (under a file lock so concurrent CLI
// processes never present the same refresh token twice).
func (c *client) refresh(ctx context.Context) (string, error) {
	if c.path != "" && !c.readOnly {
		unlock, err := lockFile(c.path, 15*time.Second)
		if err != nil {
			return "", err
		}
		defer unlock()
		// Another process may have refreshed while we waited.
		if fresh, err := LoadCredentials(c.path); err == nil && fresh.RefreshToken != c.creds.RefreshToken {
			c.creds = fresh
			if c.creds.AccessToken != "" && c.now().Add(30*time.Second).Before(c.creds.AccessExpiresAt) {
				return c.creds.AccessToken, nil
			}
		}
	}
	resp, err := c.raw(ctx, request{method: http.MethodPost, path: "/auth/refresh", headers: map[string]string{
		"Cookie":       "kase_refresh=" + c.creds.RefreshToken + "; kase_csrf=" + c.creds.CSRFToken,
		"X-CSRF-Token": c.creds.CSRFToken, "Origin": origin(c.creds.URL)}}, "")
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return "", authError{msg: "the session has ended; run `" + product.Binary + " login` again"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", decodeError(resp)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "kase_refresh" && ck.Value != "" {
			c.creds.RefreshToken = ck.Value
		}
	}
	c.creds.AccessToken, c.creds.AccessExpiresAt = out.AccessToken, c.now().Add(time.Duration(out.ExpiresIn)*time.Second)
	if c.path != "" && !c.readOnly {
		if err := SaveCredentials(c.path, c.creds); err != nil {
			return "", fmt.Errorf("save refreshed session: %w", err)
		}
	}
	return out.AccessToken, nil
}

// do sends an authenticated request and returns the successful response
// (the caller closes the body). An expired access token is refreshed once.
func (c *client) do(ctx context.Context, r request) (*http.Response, error) {
	tok := ""
	if !r.noAuth {
		var err error
		if tok, err = c.bearer(ctx); err != nil {
			return nil, err
		}
	}
	resp, err := c.raw(ctx, r, tok)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && !r.noAuth && c.creds.Kind == KindSession {
		resp.Body.Close()
		if tok, err = c.refresh(ctx); err != nil {
			return nil, err
		}
		if resp, err = c.raw(ctx, r, tok); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeError(resp)
	}
	return resp, nil
}

// json sends a request and decodes a JSON response into out (may be nil).
// It returns the raw body for --json output.
func (c *client) json(ctx context.Context, r request, out any) (json.RawMessage, error) {
	resp, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("unexpected response: %w", err)
		}
	}
	return raw, nil
}
