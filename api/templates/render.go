package templates

import (
	"bytes"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"net/url"
	"strings"
	texttemplate "text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arksnorman/open-kase/api/package/product"
)

// Kind names an email template.
type Kind string

// Email kinds.
const (
	KindTicketCreated   Kind = "ticketCreated"
	KindPublicReply     Kind = "publicReply"
	KindAgentNoteNotice Kind = "agentNoteNotice"
	KindAssignment      Kind = "assignment"
	KindStatusChanged   Kind = "statusChanged"
	KindTestEmail       Kind = "testEmail"
	// KindTicketReceived acknowledges a new ticket to its requester contact.
	KindTicketReceived Kind = "ticketReceived"
	// KindSLAAlert warns staff that an SLA target is due soon or missed.
	KindSLAAlert Kind = "slaAlert"
	// KindPasswordReset carries a single-use password reset link.
	KindPasswordReset Kind = "passwordReset"
	// KindInvitation invites a new staff user to choose a password.
	KindInvitation Kind = "invitation"
	// KindFollowupReminder asks the requester contact for a reply to a
	// ticket that is waiting for them (lifecycle automation).
	KindFollowupReminder Kind = "followupReminder"
	// KindScheduledReview tells a staff author that their scheduled message
	// was not sent automatically and needs their review.
	KindScheduledReview Kind = "scheduledReview"
	// KindScheduledCancelled tells a staff author that their scheduled
	// message was cancelled by the system or an administrator.
	KindScheduledCancelled Kind = "scheduledCancelled"
	// KindRequesterReply tells the requester contact that a public reply
	// was posted, without its content (owner decision 2026-10-31).
	KindRequesterReply Kind = "requesterReply"
	// KindHoldEnded tells the assignee that a ticket on hold is back in Open.
	KindHoldEnded Kind = "holdEnded"
)

// Versions is the current template version for each kind.
var Versions = map[Kind]int{
	KindTicketCreated:    1,
	KindPublicReply:      1,
	KindAgentNoteNotice:  1,
	KindAssignment:       1,
	KindStatusChanged:    1,
	KindTestEmail:        1,
	KindTicketReceived:   1,
	KindSLAAlert:         1,
	KindPasswordReset:    1,
	KindInvitation:       1,
	KindFollowupReminder: 1,
	KindScheduledReview:  1,
	// Cancellations by the system or an administrator (review P-ASYNC-2).
	KindScheduledCancelled: 1,
	KindRequesterReply:     1,
	KindHoldEnded:          1,
}

const layoutVersion = 1

// Limits applied to template inputs.
const (
	MaxLineRunes        = 200
	MaxSubjectRunes     = 255
	MaxBodyPreviewRunes = 4000
)

// ErrInvalidData is wrapped when template data fails validation.
var ErrInvalidData = errors.New("invalid email template data")

// Rendered is one rendered email.
type Rendered struct {
	Kind       Kind
	Version    int
	TemplateId string // e.g. "publicReply.v1", recorded in delivery history
	Subject    string
	Text       string
	HTML       string
}

// Common holds fields shared by all kinds.
type Common struct {
	// ProductName defaults to product.Name; pass the configured
	// settings.product.name to override it.
	ProductName   string
	RecipientName string
}

// Ticket is the public-safe ticket reference shown in an email.
type Ticket struct {
	Ref     string // human reference: "#" and the ticket number, such as "#42"
	Subject string
	// URL links to the ticket in the agent application for staff. For
	// requester contacts (no login) it is the configured requester link
	// (notifications.requesterLinkBaseUrl + "/" + number) or empty.
	URL string
}

// Data is implemented by the per-kind data types below.
type Data interface {
	Kind() Kind
	view() (any, error)
}

// TicketCreatedData notifies a group queue recipient about a new ticket.
type TicketCreatedData struct {
	Common
	Ticket        Ticket
	GroupName     string
	RequesterName string
	Priority      int // 1–4
}

// PublicReplyData carries a public reply. Body is the public message text
// (Markdown source, shown as plain text and never rendered as HTML).
type PublicReplyData struct {
	Common
	Ticket     Ticket
	AuthorName string
	Body       string
}

// RequesterReplyData tells a requester contact that they received a
// response. It deliberately has no body or author field: requester emails
// never carry message content.
type RequesterReplyData struct {
	Common
	Ticket Ticket
}

func (RequesterReplyData) Kind() Kind { return KindRequesterReply }

func (d RequesterReplyData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(false)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	return d, nil
}

// HoldEndedData tells the assignee that a ticket is back from hold.
type HoldEndedData struct {
	Common
	Ticket Ticket
	Reason string
}

func (HoldEndedData) Kind() Kind { return KindHoldEnded }

func (d HoldEndedData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(true)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	d.Reason = Line(d.Reason)
	if d.Reason == "" {
		return nil, invalid("hold reason is required")
	}
	return d, nil
}

// AgentNoteNoticeData is a link-only notice about an agent note. It
// deliberately has no body field.
type AgentNoteNoticeData struct {
	Common
	Ticket     Ticket
	AuthorName string
}

// AssignmentData notifies a new assignee or a target group queue.
// AssigneeName empty means the ticket moved to GroupName unassigned.
type AssignmentData struct {
	Common
	Ticket        Ticket
	GroupName     string
	AssigneeName  string
	ChangedByName string
}

// Status transitions that send email.
const (
	TransitionResolved = "resolved"
	TransitionReopened = "reopened"
	TransitionArchived = "archived"
)

// StatusChangedData reports a resolved, reopened or archived ticket.
type StatusChangedData struct {
	Common
	Ticket     Ticket
	Transition string
	StatusName string
}

// TicketReceivedData acknowledges a new ticket to its requester.
// Ticket.URL is the requester link, if configured.
type TicketReceivedData struct {
	Common
	Ticket Ticket
}

// FollowupReminderData reminds the requester that support is waiting for
// their reply. AutoResolveNotice adds the notice that the request may be
// closed without a reply. Ticket.URL is the requester link, if configured.
type FollowupReminderData struct {
	Common
	Ticket            Ticket
	AutoResolveNotice bool
}

func (FollowupReminderData) Kind() Kind { return KindFollowupReminder }

func (d FollowupReminderData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(false)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	return d, nil
}

// Scheduled review reasons and their explanations.
var scheduledReviewReasons = map[string]string{
	"newerRequesterReply":    "The requester replied after you scheduled it.",
	"statusChangeNotAllowed": "The status change you chose is no longer possible from the ticket's current status.",
	"invalidMessage":         "The message no longer meets the message rules (for example a lowered size limit).",
}

// ScheduledReviewData tells the author (staff, with a link) that a
// scheduled message was held for review. It has no body field.
type ScheduledReviewData struct {
	Common
	Ticket     Ticket
	Reason     string
	Visibility string // "public" or "agent"
}

type scheduledReviewView struct {
	ScheduledReviewData
	Explanation string
	What        string
}

func (ScheduledReviewData) Kind() Kind { return KindScheduledReview }

// scheduledCancelReasons explains the cancellations the author is told
// about; their own cancellation sends nothing.
var scheduledCancelReasons = map[string]string{
	"ticketArchived":         "The ticket was archived before the send time, and archived tickets take no new messages. Create a follow-up ticket if the requester still needs the answer.",
	"ticketMissing":          "The ticket no longer exists.",
	"authorNoAccess":         "You can no longer post this message on the ticket (your group membership or role changed).",
	"attachmentsUnavailable": "Its attachments are no longer available.",
	"cancelledByAdmin":       "A project administrator cancelled it.",
}

// ScheduledCancelNotified reports whether a cancellation reason is one the
// author is emailed about.
func ScheduledCancelNotified(reason string) bool {
	_, ok := scheduledCancelReasons[reason]
	return ok
}

// ScheduledCancelledData tells the author (staff, with a link) that a
// scheduled message was cancelled and will not be sent. It has no body.
type ScheduledCancelledData struct {
	Common
	Ticket     Ticket
	Reason     string
	Visibility string // "public" or "agent"
}

func (ScheduledCancelledData) Kind() Kind { return KindScheduledCancelled }

func (d ScheduledCancelledData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(true)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	ex, ok := scheduledCancelReasons[d.Reason]
	if !ok {
		return nil, invalid("unknown scheduled cancellation reason")
	}
	what := "public reply"
	if d.Visibility == "agent" {
		what = "agent note"
	}
	return struct {
		ScheduledCancelledData
		Explanation string
		What        string
	}{d, ex, what}, nil
}

func (d ScheduledReviewData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(true)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	ex, ok := scheduledReviewReasons[d.Reason]
	if !ok {
		return nil, invalid("unknown scheduled review reason")
	}
	what := "public reply"
	if d.Visibility == "agent" {
		what = "agent note"
	}
	return scheduledReviewView{ScheduledReviewData: d, Explanation: ex, What: what}, nil
}

// SLA target labels (SLA is first response only).
var slaTargetLabels = map[string]string{"firstResponse": "first response"}

// SLAAlertData warns staff about an SLA target (staff only, with a link).
// Target is firstResponse.
type SLAAlertData struct {
	Common
	Ticket       Ticket
	Target       string
	Breached     bool
	DueAt        time.Time
	Priority     int
	GroupName    string
	AssigneeName string
}

type slaAlertView struct {
	SLAAlertData
	TargetLabel   string
	PriorityLabel string
	DueAt         string
}

func (SLAAlertData) Kind() Kind { return KindSLAAlert }

func (d SLAAlertData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(true)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	label, ok := slaTargetLabels[d.Target]
	if !ok {
		return nil, invalid("target must be firstResponse")
	}
	if d.Priority < 1 || d.Priority > 4 {
		return nil, invalid("priority must be 1–4")
	}
	if d.DueAt.IsZero() {
		return nil, invalid("due time is required")
	}
	d.GroupName, d.AssigneeName = Line(d.GroupName), Line(d.AssigneeName)
	return slaAlertView{SLAAlertData: d, TargetLabel: label, PriorityLabel: priorityName(d.Priority),
		DueAt: d.DueAt.UTC().Format("2006-01-02 15:04 UTC")}, nil
}

// TestEmailData is the administrator test email.
type TestEmailData struct {
	Common
	RequestedBy string
	SentAt      time.Time
}

// PasswordResetData carries a single-use password reset link. URL holds a
// secret token: it is never logged and appears only in this email.
type PasswordResetData struct {
	Common
	URL       string
	ExpiresAt time.Time
}

// InvitationData invites a staff user to set a password. URL holds a
// secret single-use token.
type InvitationData struct {
	Common
	URL       string
	InvitedBy string
	Email     string // the account's sign-in address
	ExpiresAt time.Time
}

type accountLinkView struct {
	Common
	URL       string
	InvitedBy string
	Email     string
	ExpiresAt string
}

func (PasswordResetData) Kind() Kind { return KindPasswordReset }
func (InvitationData) Kind() Kind    { return KindInvitation }

func (d PasswordResetData) view() (any, error) {
	if err := checkURL(d.URL); err != nil {
		return nil, invalid("reset URL must be an absolute http(s) URL")
	}
	if d.ExpiresAt.IsZero() {
		return nil, invalid("expiry is required")
	}
	return accountLinkView{Common: d.Common.clean(), URL: d.URL, ExpiresAt: d.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")}, nil
}

func (d InvitationData) view() (any, error) {
	if err := checkURL(d.URL); err != nil {
		return nil, invalid("invitation URL must be an absolute http(s) URL")
	}
	if d.ExpiresAt.IsZero() {
		return nil, invalid("expiry is required")
	}
	return accountLinkView{Common: d.Common.clean(), URL: d.URL, InvitedBy: Line(d.InvitedBy), Email: Line(d.Email),
		ExpiresAt: d.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")}, nil
}

func (TicketCreatedData) Kind() Kind   { return KindTicketCreated }
func (PublicReplyData) Kind() Kind     { return KindPublicReply }
func (AgentNoteNoticeData) Kind() Kind { return KindAgentNoteNotice }
func (AssignmentData) Kind() Kind      { return KindAssignment }
func (StatusChangedData) Kind() Kind   { return KindStatusChanged }
func (TestEmailData) Kind() Kind       { return KindTestEmail }
func (TicketReceivedData) Kind() Kind  { return KindTicketReceived }

func (d TicketReceivedData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(false)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	return d, nil
}

// Views passed to templates.

type ticketCreatedView struct {
	TicketCreatedData
	PriorityLabel string
}

type publicReplyView struct {
	PublicReplyData
	Truncated bool
}

type statusChangedView struct {
	StatusChangedData
	TransitionTitle string
}

type testEmailView struct {
	Common
	RequestedBy string
	SentAt      string
}

func (d TicketCreatedData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(true)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	d.GroupName = Line(d.GroupName)
	d.RequesterName = Line(d.RequesterName)
	if d.GroupName == "" {
		return nil, invalid("group name is required")
	}
	if d.Priority < 1 || d.Priority > 4 {
		return nil, invalid("priority must be 1–4")
	}
	return ticketCreatedView{d, priorityName(d.Priority)}, nil
}

func (d PublicReplyData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(false)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	d.AuthorName = Line(d.AuthorName)
	body, truncated := Preview(d.Body, MaxBodyPreviewRunes)
	if strings.TrimSpace(body) == "" {
		return nil, invalid("reply body is required")
	}
	d.Body = body
	return publicReplyView{d, truncated}, nil
}

func (d AgentNoteNoticeData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(true)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	d.AuthorName = Line(d.AuthorName)
	return d, nil
}

func (d AssignmentData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(true)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	d.GroupName = Line(d.GroupName)
	d.AssigneeName = Line(d.AssigneeName)
	d.ChangedByName = Line(d.ChangedByName)
	if d.GroupName == "" {
		return nil, invalid("group name is required")
	}
	return d, nil
}

func (d StatusChangedData) view() (any, error) {
	d.Common = d.Common.clean()
	t, err := d.Ticket.clean(false)
	if err != nil {
		return nil, err
	}
	d.Ticket = t
	d.StatusName = Line(d.StatusName)
	var title string
	switch d.Transition {
	case TransitionResolved:
		title = "Ticket resolved"
	case TransitionReopened:
		title = "Ticket reopened"
	case TransitionArchived:
		title = "Ticket archived"
	default:
		return nil, invalid("transition must be resolved, reopened or archived")
	}
	return statusChangedView{d, title}, nil
}

func (d TestEmailData) view() (any, error) {
	if d.SentAt.IsZero() {
		return nil, invalid("sent time is required")
	}
	return testEmailView{
		Common:      d.Common.clean(),
		RequestedBy: Line(d.RequestedBy),
		SentAt:      d.SentAt.UTC().Format("2006-01-02 15:04:05 UTC"),
	}, nil
}

func (c Common) clean() Common {
	c.ProductName = Line(c.ProductName)
	if c.ProductName == "" {
		c.ProductName = product.Name
	}
	c.RecipientName = Line(c.RecipientName)
	return c
}

func (t Ticket) clean(requireURL bool) (Ticket, error) {
	t.Ref = Line(t.Ref)
	t.Subject = Line(t.Subject)
	if t.Ref == "" || t.Subject == "" {
		return t, invalid("ticket reference and subject are required")
	}
	if t.URL == "" {
		if requireURL {
			return t, invalid("ticket URL is required for staff notifications")
		}
		return t, nil
	}
	if err := checkURL(t.URL); err != nil {
		return t, err
	}
	return t, nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return invalid("ticket URL must be an absolute http(s) URL without credentials")
	}
	if strings.ContainsFunc(raw, unicode.IsSpace) || strings.ContainsFunc(raw, unicode.IsControl) {
		return invalid("ticket URL contains whitespace")
	}
	return nil
}

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidData, msg) }

// Line makes untrusted text safe for a single line (subject, names): control
// characters and line breaks become spaces, invisible format characters
// (such as bidirectional overrides) are removed, whitespace is collapsed,
// and the result is limited to MaxLineRunes.
func Line(s string) string {
	return truncate(oneLine(s), MaxLineRunes)
}

func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.Map(func(r rune) rune {
		if isInvisibleFormat(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// isInvisibleFormat reports Unicode format characters (category Cf) other
// than the zero-width joiner used in emoji sequences.
func isInvisibleFormat(r rune) bool {
	return unicode.Is(unicode.Cf, r) && r != '\u200d'
}

// Preview makes untrusted multi-line text safe for a plain-text body:
// newlines are normalized, other control characters removed, and the text
// is limited to max runes. It reports whether text was cut.
func Preview(s string, max int) (string, bool) {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\u2028' || r == '\u2029':
			return '\n'
		case unicode.IsControl(r) || isInvisibleFormat(r):
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	return truncate(s, max), true
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

type kindTemplates struct {
	subject *texttemplate.Template
	text    *texttemplate.Template
	html    *htmltemplate.Template
}

// Renderer renders embedded templates. It is immutable and safe for
// concurrent use.
type Renderer struct {
	kinds map[Kind]kindTemplates
}

// NewRenderer parses every template and fails if one is missing or invalid.
func NewRenderer() (*Renderer, error) {
	r := &Renderer{kinds: make(map[Kind]kindTemplates, len(Versions))}
	textLayout := fmt.Sprintf("email/layout.v%d.txt", layoutVersion)
	htmlLayout := fmt.Sprintf("email/layout.v%d.html", layoutVersion)
	for kind, v := range Versions {
		base := fmt.Sprintf("email/%s.v%d", kind, v)
		subject, err := texttemplate.New(fmt.Sprintf("%s.v%d.subject.txt", kind, v)).
			Option("missingkey=error").ParseFS(FS, base+".subject.txt")
		if err != nil {
			return nil, fmt.Errorf("parse %s subject: %w", kind, err)
		}
		text, err := texttemplate.New("layout").Option("missingkey=error").ParseFS(FS, textLayout, base+".txt")
		if err != nil {
			return nil, fmt.Errorf("parse %s text: %w", kind, err)
		}
		html, err := htmltemplate.New("layout").Option("missingkey=error").ParseFS(FS, htmlLayout, base+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s html: %w", kind, err)
		}
		r.kinds[kind] = kindTemplates{subject: subject, text: text, html: html}
	}
	return r, nil
}

// Render validates and cleans d, then renders subject, text and HTML.
func (r *Renderer) Render(d Data) (Rendered, error) {
	kind := d.Kind()
	t, ok := r.kinds[kind]
	if !ok {
		return Rendered{}, fmt.Errorf("unknown email template %q", kind)
	}
	v, err := d.view()
	if err != nil {
		return Rendered{}, err
	}
	var subject, text, html bytes.Buffer
	if err := t.subject.Execute(&subject, v); err != nil {
		return Rendered{}, fmt.Errorf("render %s subject: %w", kind, err)
	}
	if err := t.text.ExecuteTemplate(&text, "layout", v); err != nil {
		return Rendered{}, fmt.Errorf("render %s text: %w", kind, err)
	}
	if err := t.html.ExecuteTemplate(&html, "layout", v); err != nil {
		return Rendered{}, fmt.Errorf("render %s html: %w", kind, err)
	}
	subj := truncate(oneLine(subject.String()), MaxSubjectRunes)
	return Rendered{
		Kind:       kind,
		Version:    Versions[kind],
		TemplateId: fmt.Sprintf("%s.v%d", kind, Versions[kind]),
		Subject:    subj,
		Text:       tidyText(text.String()),
		HTML:       html.String(),
	}, nil
}

// tidyText collapses runs of blank lines left by conditional sections.
func tidyText(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n") + "\n"
}

// priorityName is the label of a priority: Critical, High, Normal or Low
// (owner decision 2026-10-31: names, not "Priority N").
func priorityName(p int) string {
	switch p {
	case 1:
		return "Critical"
	case 2:
		return "High"
	case 3:
		return "Normal"
	case 4:
		return "Low"
	}
	return "Normal"
}
