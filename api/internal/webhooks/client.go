// Package webhooks delivers and schedules outbound webhook requests.
//
// Requests are not signed (owner decision 2026-10-31). Receivers
// authenticate deliveries through HTTPS plus the endpoint's configured
// authentication (Basic or Bearer token), custom headers or a client
// certificate (mutual TLS), and de-duplicate by the event ID header.
//
// Delivery: Client refuses non-HTTPS URLs (unless development mode allows
// HTTP), never follows redirects, validates every resolved IP address at
// dial time (DNS-rebinding safe) and bounds time and response size. Private
// networks are allowed; loopback only in development; link-local,
// reserved and cloud metadata addresses never. Certificate verification is
// never disabled; an endpoint may add trusted CA certificates.
package webhooks

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/arksnorman/open-kase/api/package/product"
)

// Header names sent with every delivery.
const (
	HeaderEventID   = product.HeaderPrefix + "Event-Id"
	HeaderEventType = product.HeaderPrefix + "Event-Type"
)

// Policy restricts webhook destinations and bounds requests.
type Policy struct {
	// AllowHTTP permits http:// URLs. Only for development installations.
	AllowHTTP bool
	// AllowLoopback permits loopback addresses. Only for development
	// installations (local test receivers).
	AllowLoopback bool
	// Timeout bounds the whole request including reading the response.
	Timeout time.Duration
	// MaxResponseBytes limits how much of the response body is read.
	MaxResponseBytes int64
	// MaxRetryAfter caps a receiver's Retry-After value.
	MaxRetryAfter time.Duration
	// RootCAs replaces the system roots (tests). Certificates are always
	// verified. An endpoint's own CA certificates are added to these roots.
	RootCAs *x509.CertPool
}

// Policy defaults.
const (
	DefaultTimeout          = 10 * time.Second
	DefaultMaxResponseBytes = 64 << 10
	DefaultMaxRetryAfter    = 12 * time.Hour
	MaxURLLength            = 2048
	snippetBytes            = 256
)

func (p Policy) withDefaults() Policy {
	if p.Timeout <= 0 {
		p.Timeout = DefaultTimeout
	}
	if p.MaxResponseBytes <= 0 {
		p.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if p.MaxRetryAfter <= 0 {
		p.MaxRetryAfter = DefaultMaxRetryAfter
	}
	return p
}

// ErrDestinationBlocked is wrapped when a URL or resolved address is not an
// allowed destination.
var ErrDestinationBlocked = errors.New("webhook destination is not allowed")

// alwaysBlocked are never reachable.
var alwaysBlocked = []netip.Prefix{
	netip.MustParsePrefix("169.254.169.254/32"), // AWS/GCP/Azure/OpenStack metadata
	netip.MustParsePrefix("169.254.170.2/32"),   // AWS ECS task metadata
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IPv6 metadata
}

// blockedRanges are special-purpose ranges that are never reachable, in
// addition to the netip predicates checked in blocked. Private networks
// (RFC 1918, IPv6 unique local, CGNAT) are allowed: only administrators
// configure webhooks, and self-hosted receivers often live on the LAN.
var blockedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 can reach internal IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("100::/64"),       // discard
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("2002::/16"),      // 6to4 can embed internal IPv4
}

func blocked(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || !ip.IsGlobalUnicast() {
		return true
	}
	for _, p := range blockedRanges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// CheckIP reports whether ip is an allowed destination under p.
func (p Policy) CheckIP(ip netip.Addr) error {
	ip = ip.Unmap().WithZone("")
	for _, b := range alwaysBlocked {
		if b.Contains(ip) {
			return fmt.Errorf("%w: %s is a metadata address", ErrDestinationBlocked, ip)
		}
	}
	if ip.IsLoopback() {
		if p.AllowLoopback {
			return nil
		}
		return fmt.Errorf("%w: %s is a loopback address", ErrDestinationBlocked, ip)
	}
	if blocked(ip) {
		return fmt.Errorf("%w: %s is in a reserved range", ErrDestinationBlocked, ip)
	}
	return nil
}

// CheckURL validates an endpoint URL without resolving it. Hostnames are
// checked again for every connection at dial time.
func (p Policy) CheckURL(raw string) (*url.URL, error) {
	if len(raw) > MaxURLLength || !utf8.ValidString(raw) {
		return nil, fmt.Errorf("%w: URL is too long or not UTF-8", ErrDestinationBlocked)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: URL cannot be parsed", ErrDestinationBlocked)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return nil, fmt.Errorf("%w: HTTPS is required", ErrDestinationBlocked)
		}
	default:
		return nil, fmt.Errorf("%w: scheme must be https", ErrDestinationBlocked)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: URL must not contain credentials", ErrDestinationBlocked)
	}
	host := u.Hostname()
	if host == "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: URL has no host", ErrDestinationBlocked)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%w: invalid port", ErrDestinationBlocked)
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := p.CheckIP(ip); err != nil {
			return nil, err
		}
	} else if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".localhost") {
		// Resolves to loopback; checked again at dial time either way.
		if err := p.CheckIP(netip.MustParseAddr("127.0.0.1")); err != nil {
			return nil, err
		}
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// Client delivers webhook requests.
type Client struct {
	policy    Policy
	transport *http.Transport
	http      *http.Client
}

// NewClient returns a client enforcing p.
func NewClient(p Policy) *Client {
	p = p.withDefaults()
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		// Control runs after DNS resolution with the concrete IP about to be
		// connected, for every attempt (including Happy Eyeballs fallbacks),
		// so a rebinding DNS answer cannot bypass the check.
		Control: func(network, address string, _ syscall.RawConn) error {
			if network != "tcp4" && network != "tcp6" {
				return fmt.Errorf("%w: network %s", ErrDestinationBlocked, network)
			}
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: unparseable address", ErrDestinationBlocked)
			}
			return p.CheckIP(ap.Addr())
		},
	}
	transport := &http.Transport{
		Proxy:                  nil, // an environment proxy would bypass the IP checks
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.RootCAs},
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  p.Timeout,
		ExpectContinueTimeout:  time.Second,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        60 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		ForceAttemptHTTP2:      true,
	}
	return &Client{
		policy:    p,
		transport: transport,
		http: &http.Client{
			Transport: transport,
			Timeout:   p.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Outcome classifies a delivery attempt.
type Outcome string

const (
	OutcomeDelivered Outcome = "delivered"
	// OutcomeRetry: the attempt failed and may succeed later.
	OutcomeRetry Outcome = "retry"
	// OutcomePermanent: retrying will not help (blocked destination, 410).
	OutcomePermanent Outcome = "permanent"
)

// ErrorClass is a short, secret-free reason shown in delivery history.
type ErrorClass string

const (
	ErrorNone        ErrorClass = ""
	ErrorBlocked     ErrorClass = "destinationBlocked"
	ErrorTimeout     ErrorClass = "timeout"
	ErrorTLS         ErrorClass = "tls"
	ErrorNetwork     ErrorClass = "network"
	ErrorRedirect    ErrorClass = "redirectNotFollowed"
	ErrorHTTPStatus  ErrorClass = "httpStatus"
	ErrorRateLimited ErrorClass = "rateLimited"
	ErrorGone        ErrorClass = "gone"
	// ErrorConfig: the endpoint's stored configuration cannot be used
	// (undecryptable credential, unusable certificate).
	ErrorConfig ErrorClass = "configuration"
)

// Header is one extra request header configured for an endpoint.
type Header struct {
	Name  string
	Value string
}

// TLSConfig customizes certificate handling for one endpoint.
type TLSConfig struct {
	// CAPEM holds CA certificates (PEM) trusted in addition to the roots
	// (the system roots, or Policy.RootCAs).
	CAPEM []byte
	// ClientCert is presented for mutual TLS when the receiver asks.
	ClientCert *tls.Certificate
}

// Request is one delivery attempt. Body must be the exact stored bytes of
// the envelope; retries reuse the same EventID and Body.
type Request struct {
	URL       string
	EventId   string
	EventType string
	Body      []byte
	// Headers are the endpoint's extra headers (validated when saved; the
	// fixed headers always win).
	Headers []Header
	// Authorization is the complete Authorization header value, or "".
	Authorization string
	// TLS is nil for the default verification.
	TLS *TLSConfig
}

// Result describes an attempt. It never contains secrets or request bodies.
type Result struct {
	Outcome    Outcome
	ErrorClass ErrorClass
	StatusCode int
	// RetryAfter is the receiver's capped Retry-After, zero when absent.
	RetryAfter time.Duration
	Duration   time.Duration
	// ResponseSnippet is the beginning of the response body (at most 256
	// bytes, valid UTF-8) for delivery history.
	ResponseSnippet string
	Err             error
}

// Deliver sends one POST.
func (c *Client) Deliver(ctx context.Context, r Request) Result {
	return c.deliver(ctx, r)
}

// httpClient returns the client for an endpoint: the shared one, or one
// with the endpoint's TLS settings on a copy of the guarded transport
// (same dial checks, no proxy, no redirects).
func (c *Client) httpClient(t *TLSConfig) (*http.Client, func(), error) {
	if t == nil || (len(t.CAPEM) == 0 && t.ClientCert == nil) {
		return c.http, func() {}, nil
	}
	cfg := c.transport.TLSClientConfig.Clone()
	if len(t.CAPEM) > 0 {
		var roots *x509.CertPool
		if c.policy.RootCAs != nil {
			roots = c.policy.RootCAs.Clone()
		} else if sys, err := x509.SystemCertPool(); err == nil {
			roots = sys
		} else {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(t.CAPEM) {
			return nil, nil, errors.New("the endpoint's CA certificate cannot be parsed")
		}
		cfg.RootCAs = roots
	}
	if t.ClientCert != nil {
		cfg.Certificates = []tls.Certificate{*t.ClientCert}
	}
	tr := c.transport.Clone()
	tr.TLSClientConfig = cfg
	hc := *c.http
	hc.Transport = tr
	return &hc, tr.CloseIdleConnections, nil
}

func (c *Client) deliver(ctx context.Context, r Request) Result {
	u, err := c.policy.CheckURL(r.URL)
	if err != nil {
		return Result{Outcome: OutcomePermanent, ErrorClass: ErrorBlocked, Err: err}
	}
	if r.EventId == "" {
		return Result{Outcome: OutcomePermanent, ErrorClass: ErrorConfig, Err: errors.New("webhook event id is required")}
	}
	hc, done, err := c.httpClient(r.TLS)
	if err != nil {
		return Result{Outcome: OutcomePermanent, ErrorClass: ErrorConfig, Err: err}
	}
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(r.Body))
	if err != nil {
		return Result{Outcome: OutcomePermanent, ErrorClass: ErrorBlocked, Err: err}
	}
	for _, h := range r.Headers {
		req.Header.Set(h.Name, h.Value)
	}
	req.Header.Set("Content-Type", "application/json")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", product.Slug+"-webhooks/"+product.Version)
	}
	req.Header.Set(HeaderEventID, r.EventId)
	if r.EventType != "" {
		req.Header.Set(HeaderEventType, r.EventType)
	}
	if r.Authorization != "" {
		req.Header.Set("Authorization", r.Authorization)
	}

	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, c.policy.MaxResponseBytes))
	// Time to the end of the response; excludes request setup and CA parsing.
	res := Result{StatusCode: resp.StatusCode, ResponseSnippet: snippet(body), Duration: time.Since(start)}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Outcome = OutcomeDelivered
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		res.Outcome, res.ErrorClass = OutcomeRetry, ErrorRedirect
	case resp.StatusCode == http.StatusGone:
		res.Outcome, res.ErrorClass = OutcomePermanent, ErrorGone
	case resp.StatusCode == http.StatusTooManyRequests:
		res.Outcome, res.ErrorClass = OutcomeRetry, ErrorRateLimited
	default:
		res.Outcome, res.ErrorClass = OutcomeRetry, ErrorHTTPStatus
	}
	if res.Outcome == OutcomeRetry {
		if d, ok := ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now(), c.policy.MaxRetryAfter); ok {
			res.RetryAfter = d
		}
	}
	if res.Outcome != OutcomeDelivered {
		res.Err = fmt.Errorf("webhook endpoint responded %d", resp.StatusCode)
	}
	return res
}

func classifyTransportError(err error) Result {
	res := Result{Outcome: OutcomeRetry, ErrorClass: ErrorNetwork, Err: err}
	var netErr net.Error
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	switch {
	case errors.Is(err, ErrDestinationBlocked):
		res.Outcome, res.ErrorClass = OutcomePermanent, ErrorBlocked
	case errors.As(err, &certErr), errors.As(err, &recordErr), errors.As(err, &alertErr):
		res.ErrorClass = ErrorTLS
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		res.ErrorClass = ErrorTimeout
	}
	return res
}

func snippet(b []byte) string {
	if len(b) > snippetBytes {
		b = b[:snippetBytes]
	}
	return strings.ToValidUTF8(string(b), "")
}

// ParseRetryAfter parses a Retry-After value (delta-seconds or HTTP-date),
// capped at max. Past dates yield zero.
func ParseRetryAfter(v string, now time.Time, max time.Duration) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0, false
		}
		if n > int64(max/time.Second) {
			return max, true
		}
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
		if d < 0 {
			d = 0
		}
	} else {
		return 0, false
	}
	if d > max {
		d = max
	}
	return d, true
}
