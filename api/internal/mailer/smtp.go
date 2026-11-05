package mailer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/textproto"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"
)

// TLSPolicy selects how the SMTP connection is protected.
type TLSPolicy string

const (
	// TLSStartTLS upgrades with STARTTLS and fails when the server does not
	// offer it (mandatory). This is the default.
	TLSStartTLS TLSPolicy = "starttls"
	// TLSStartTLSOpportunistic upgrades with STARTTLS when offered and
	// otherwise continues in plain text. Not allowed with authentication.
	TLSStartTLSOpportunistic TLSPolicy = "starttlsOpportunistic"
	// TLSImplicit connects with TLS from the first byte (port 465).
	TLSImplicit TLSPolicy = "tls"
	// TLSNone never encrypts. Only for a local relay; not allowed with
	// authentication.
	TLSNone TLSPolicy = "none"
)

// ParseTLSPolicy maps a stored settings value to a policy.
func ParseTLSPolicy(s string) (TLSPolicy, error) {
	switch p := TLSPolicy(s); p {
	case TLSStartTLS, TLSStartTLSOpportunistic, TLSImplicit, TLSNone:
		return p, nil
	case "":
		return TLSStartTLS, nil
	}
	return "", fmt.Errorf("unknown SMTP TLS policy %q", s)
}

// DefaultTimeout bounds each SMTP command when Config.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// Config is the narrow configuration view the SMTP adapter needs. Password
// is the decrypted secret; it is never logged or included in errors.
type Config struct {
	Host      string
	Port      int
	Username  string
	Password  string
	From      Address
	TLSPolicy TLSPolicy
	// Timeout bounds dialing and each SMTP command.
	Timeout time.Duration
	// HELO overrides the EHLO name (default: the local hostname).
	HELO string
	// RootCAs replaces the system roots, for relays with a private CA.
	RootCAs *x509.CertPool
	// InsecureSkipVerify skips server certificate verification (an explicit
	// administrator choice for self-signed relays; the connection is then open
	// to interception). RootCAs and InsecureSkipVerify are ignored with TLSNone.
	InsecureSkipVerify bool
}

// RootsWithPEM returns the system roots plus the certificates of a PEM bundle.
func RootsWithPEM(bundle string) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM([]byte(bundle)) {
		return nil, errors.New("the SMTP CA certificate cannot be parsed")
	}
	return roots, nil
}

// tlsConfig is the TLS configuration for the connection (nil roots and no
// skipping with TLSNone, where TLS is never used).
func (c Config) tlsConfig() *tls.Config {
	cfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	if c.TLSPolicy != TLSNone {
		cfg.RootCAs = c.RootCAs
		cfg.InsecureSkipVerify = c.InsecureSkipVerify //nolint:gosec // explicit administrator opt-in
	}
	return cfg
}

// Validate checks the configuration without contacting the server.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" || strings.ContainsAny(c.Host, " /\r\n") {
		return errors.New("smtp host is required")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("smtp port must be between 1 and 65535")
	}
	if _, err := ParseTLSPolicy(string(c.TLSPolicy)); err != nil {
		return err
	}
	if c.Username != "" && (c.TLSPolicy == TLSNone || c.TLSPolicy == TLSStartTLSOpportunistic) {
		return errors.New("authenticated SMTP requires mandatory STARTTLS or implicit TLS")
	}
	if c.Username == "" && c.Password != "" {
		return errors.New("smtp password set without a username")
	}
	if err := validateAddress("from", c.From); err != nil {
		return err
	}
	if c.Timeout < 0 {
		return errors.New("smtp timeout must not be negative")
	}
	return nil
}

// SMTP sends each message over a new connection. It is safe for concurrent
// use; the configuration is immutable after construction.
type SMTP struct {
	cfg Config
	now func() time.Time
}

// NewSMTP validates cfg and returns the adapter.
func NewSMTP(cfg Config) (*SMTP, error) {
	if cfg.TLSPolicy == "" {
		cfg.TLSPolicy = TLSStartTLS
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &SMTP{cfg: cfg, now: time.Now}, nil
}

// Send delivers msg. Errors are *Error values classified for retrying.
func (s *SMTP) Send(ctx context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return &Error{Class: ClassPermanent, Phase: PhaseValidate, Err: err}
	}
	m, err := s.build(msg)
	if err != nil {
		return &Error{Class: ClassPermanent, Phase: PhaseValidate, Err: err}
	}
	client, err := s.client(ctx)
	if err != nil {
		return &Error{Class: ClassPermanent, Phase: PhaseConfig, Err: err}
	}
	sc, err := client.DialToSMTPClientWithContext(ctx)
	if err != nil {
		return classifyConnect(ctx, err)
	}
	defer func() { _ = client.CloseWithSMTPClient(sc) }()
	if err := client.SendWithSMTPClient(sc, m); err != nil {
		if m.IsDelivered() {
			// Accepted at end of DATA; only the following RSET failed.
			return nil
		}
		return classifySend(err)
	}
	return nil
}

func (s *SMTP) client(ctx context.Context) (*gomail.Client, error) {
	timeout := s.cfg.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < timeout {
			timeout = rem
		}
	}
	tlsCfg := s.cfg.tlsConfig()
	opts := []gomail.Option{
		gomail.WithPort(s.cfg.Port),
		gomail.WithTimeout(timeout),
		gomail.WithTLSConfig(tlsCfg),
	}
	switch s.cfg.TLSPolicy {
	case TLSImplicit:
		opts = append(opts, gomail.WithSSL(), gomail.WithTLSPolicy(gomail.NoTLS))
	case TLSStartTLSOpportunistic:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSOpportunistic))
	case TLSNone:
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	if s.cfg.HELO != "" {
		opts = append(opts, gomail.WithHELO(s.cfg.HELO))
	}
	if s.cfg.Username != "" {
		// Auto-discovery picks the strongest mechanism the server offers.
		// Authentication only runs over TLS (enforced by Config.Validate).
		opts = append(opts, gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(s.cfg.Username), gomail.WithPassword(s.cfg.Password))
	}
	return gomail.NewClient(s.cfg.Host, opts...)
}

func (s *SMTP) build(msg Message) (*gomail.Msg, error) {
	m := gomail.NewMsg(gomail.WithNoDefaultUserAgent())
	if err := m.FromFormat(s.cfg.From.Name, s.cfg.From.Email); err != nil {
		return nil, err
	}
	if err := m.AddToFormat(msg.To.Name, msg.To.Email); err != nil {
		return nil, err
	}
	if msg.ReplyTo != nil {
		if err := m.ReplyToFormat(msg.ReplyTo.Name, msg.ReplyTo.Email); err != nil {
			return nil, err
		}
	}
	m.Subject(msg.Subject)
	if msg.MessageId != "" {
		m.SetMessageIDWithValue(msg.MessageId)
	} else {
		m.SetMessageID()
	}
	m.SetDateWithValue(s.now())
	m.SetGenHeader("Auto-Submitted", "auto-generated")
	m.SetGenHeader("X-Auto-Response-Suppress", "All")
	for name, value := range msg.Headers {
		m.SetGenHeader(gomail.Header(name), value)
	}
	m.SetBodyString(gomail.TypeTextPlain, msg.Text)
	if msg.HTML != "" {
		m.AddAlternativeString(gomail.TypeTextHTML, msg.HTML)
	}
	return m, nil
}

// classifyConnect handles failures before MAIL FROM: nothing was sent.
func classifyConnect(ctx context.Context, err error) error {
	e := &Error{Class: ClassTemporary, Phase: PhaseConnect, Err: err}
	var tpe *textproto.Error
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	switch {
	case errors.As(err, &tpe):
		e.Code = tpe.Code
		if tpe.Code >= 500 {
			e.Class = ClassPermanent
		}
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalidCert):
		e.Class = ClassPermanent
	case errors.Is(err, gomail.ErrNoSupportedAuthDiscovered),
		strings.Contains(err.Error(), "does not support STARTTLS"),
		strings.Contains(err.Error(), "does not support SMTP AUTH"):
		e.Class = ClassPermanent
	}
	if ctx.Err() != nil {
		e.Class = ClassTemporary
	}
	return e
}

// classifySend handles failures from MAIL FROM onwards.
func classifySend(err error) error {
	var se *gomail.SendError
	if !errors.As(err, &se) {
		return &Error{Class: ClassUnknownOutcome, Phase: PhaseCommit, Err: err}
	}
	code := se.ErrorCode()
	if code == 0 {
		var tpe *textproto.Error
		if errors.As(err, &tpe) {
			code = tpe.Code
		}
	}
	byCode := func(fallback Class) Class {
		switch {
		case code >= 500:
			return ClassPermanent
		case code >= 400:
			return ClassTemporary
		}
		return fallback
	}
	e := &Error{Phase: PhaseEnvelope, Code: code, Err: err}
	switch se.Reason {
	case gomail.ErrGetSender, gomail.ErrGetRcpts, gomail.ErrNoUnencoded:
		e.Phase, e.Class = PhaseValidate, ClassPermanent
	case gomail.ErrConnCheck, gomail.ErrSMTPMailFrom, gomail.ErrSMTPRcptTo:
		e.Class = byCode(ClassTemporary)
	case gomail.ErrSMTPData, gomail.ErrWriteContent:
		// The terminating "." was not sent, so the server discards the
		// partial message.
		e.Phase, e.Class = PhaseData, byCode(ClassTemporary)
	case gomail.ErrSMTPDataClose:
		// The whole message was sent. Without a reply code the server may
		// have accepted it.
		e.Phase, e.Class = PhaseCommit, byCode(ClassUnknownOutcome)
	default:
		e.Phase, e.Class = PhaseCommit, ClassUnknownOutcome
	}
	return e
}

// Compile-time interface checks.
var (
	_ Mailer = (*SMTP)(nil)
	_ Mailer = (*Fake)(nil)
)
