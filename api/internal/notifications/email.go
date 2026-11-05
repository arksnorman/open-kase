package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/mailer"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/templates"
)

// UserInfo is a staff user's notification-relevant data.
type UserInfo struct {
	Id          string
	WorkspaceId string
	Name        string
	Email       string
	Active      bool
	Admin       bool
	MuteWatcher bool
	MuteQueue   bool
}

// ContactInfo is a requester contact's address.
type ContactInfo struct {
	Id    string
	Name  string
	Email string
}

// People resolves recipients.
type People interface {
	User(ctx context.Context, workspaceID, id string) (UserInfo, error)
	Contact(ctx context.Context, projectID, id string) (ContactInfo, error)
}

// Tickets is the ticket access the workers need (tickets.Service).
type Tickets interface {
	Get(ctx context.Context, scope *access.ProjectScope, ref string) (tickets.Ticket, error)
	GetMessage(ctx context.Context, scope *access.ProjectScope, ticketRef, messageID string) (tickets.Ticket, tickets.Message, error)
	SystemTicket(ctx context.Context, projectID, ticketID string) (tickets.Ticket, error)
	SystemMessage(ctx context.Context, ticketID, messageID string) (tickets.Message, error)
	OrganisationRef(ctx context.Context, projectID, id string) (tickets.OrgRef, error)
}

// Authorizer authorizes a principal (access.Service).
type Authorizer interface {
	Scope(ctx context.Context, p access.Principal) (*access.ProjectScope, error)
}

// Opener decrypts database-held secrets (secrets.Keyring).
type Opener interface {
	Open(ciphertext, context string) ([]byte, error)
}

// MailerFactory builds a Mailer from the current SMTP configuration.
type MailerFactory func(cfg mailer.Config) (mailer.Mailer, error)

// SMTPMailer is the production MailerFactory.
func SMTPMailer(cfg mailer.Config) (mailer.Mailer, error) { return mailer.NewSMTP(cfg) }

// EmailDeps wires the email workers.
type EmailDeps struct {
	Jobs       jobs.Store
	Deliveries Deliveries
	Events     Events
	Dir        Directory
	People     People
	Tickets    Tickets
	Access     Authorizer
	Settings   func() *settings.Snapshot
	Keys       Opener
	Mailers    MailerFactory
	Renderer   *templates.Renderer
	Now        func() time.Time
	// Accounts enables password-reset and invitation emails.
	Accounts Accounts
}

// Email delivers notification and test emails.
type Email struct{ d EmailDeps }

// NewEmail returns the email job handlers.
func NewEmail(d EmailDeps) *Email {
	if d.Mailers == nil {
		d.Mailers = SMTPMailer
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
	}
	return &Email{d: d}
}

// skip is a deliberate non-delivery (recipient lost access, muted, ...).
type skip struct{ class string }

func (s skip) Error() string { return "skipped: " + s.class }

func skipped(class string) error { return skip{class: class} }

// denied reports whether err is an authorization outcome (the recipient may
// no longer see the project, ticket or message).
func denied(err error) bool {
	var fe *fault.Error
	return errors.As(err, &fe) && (fe.Kind == fault.KindNotFound || fe.Kind == fault.KindForbidden)
}

// HandleNotification is the jobs.Handler for KindEmail.
func (e *Email) HandleNotification(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	return e.run(ctx, j, e.buildNotification)
}

// HandleTest is the jobs.Handler for KindTestEmail.
func (e *Email) HandleTest(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	return e.run(ctx, j, e.buildTest)
}

type builder func(ctx context.Context, j jobs.Job, d Delivery, snap *settings.Snapshot) (mailer.Message, error)

func (e *Email) run(ctx context.Context, j jobs.Job, build builder) jobs.Result {
	d, err := e.d.Deliveries.GetDelivery(ctx, j.Id)
	if errors.Is(err, ErrNotFound) {
		return jobs.Result{Outcome: jobs.Dead, ErrorClass: "deliveryMissing", Error: "no delivery record for this job"}
	}
	if err != nil {
		return jobs.Result{Outcome: jobs.Retry, ErrorClass: "database", Error: err.Error()}
	}
	if Terminal(d.Status) {
		return jobs.Result{Outcome: jobs.Done}
	}
	snap := e.d.Settings()
	if snap == nil {
		return jobs.Result{Outcome: jobs.Retry, ErrorClass: "settingsUnavailable", Error: "settings are not loaded yet"}
	}
	now := e.d.Now()
	upd := DeliveryUpdate{Attempts: j.Attempts, LastAttemptAt: now}
	finish := func(status, class, msg string, outcome jobs.Outcome) jobs.Result {
		return jobs.Result{Outcome: outcome, ErrorClass: class, Error: msg, Commit: func(ctx context.Context, f jobs.Finish) error {
			u := upd
			u.Status, u.ErrorClass, u.Error = status, class, msg
			switch f.State {
			case jobs.StateQueued:
				u.Status, u.NextAttemptAt = StatusRetrying, &f.RunAt
			case jobs.StateDead:
				if !Terminal(u.Status) || u.Status == StatusSent {
					u.Status = StatusFailed
				}
			}
			return e.d.Deliveries.UpdateDelivery(ctx, j.Id, u)
		}}
	}

	smtp := snap.SMTP()
	if !smtp.Enabled || smtp.Host == "" || smtp.FromAddress == "" {
		return finish(StatusSkippedUnconfigured, "smtpNotConfigured", "email delivery is not configured", jobs.Done)
	}
	msg, err := build(ctx, j, d, snap)
	var sk skip
	switch {
	case errors.As(err, &sk):
		return finish(StatusSkipped, sk.class, "", jobs.Done)
	case errors.Is(err, templates.ErrInvalidData), errors.Is(err, mailer.ErrInvalidMessage):
		return finish(StatusFailed, "invalidMessage", err.Error(), jobs.Dead)
	case err != nil:
		return finish(StatusRetrying, "lookupFailed", err.Error(), jobs.Retry)
	}
	upd.Address, upd.MessageId = msg.To.Email, msg.MessageId

	cfg, err := e.smtpConfig(smtp)
	if err != nil {
		return finish(StatusFailed, "smtpConfig", err.Error(), jobs.Dead)
	}
	m, err := e.d.Mailers(cfg)
	if err != nil {
		return finish(StatusFailed, "smtpConfig", err.Error(), jobs.Dead)
	}
	sendErr := m.Send(ctx, msg)
	switch mailer.ClassOf(sendErr) {
	case "":
		return finish(StatusSent, "", "", jobs.Done)
	case mailer.ClassTemporary:
		return finish(StatusRetrying, "smtpTemporary", sendErr.Error(), jobs.Retry)
	case mailer.ClassPermanent:
		return finish(StatusFailed, "smtpPermanent", sendErr.Error(), jobs.Dead)
	default:
		// The server may have accepted it: never retry automatically.
		return finish(StatusUnknownOutcome, "smtpUnknownOutcome", sendErr.Error(), jobs.Done)
	}
}

func (e *Email) smtpConfig(s settings.SMTPSettings) (mailer.Config, error) {
	pw := ""
	if s.PasswordCiphertext != "" {
		plain, err := e.d.Keys.Open(s.PasswordCiphertext, settings.SMTPPasswordContext)
		if err != nil {
			return mailer.Config{}, errors.New("the stored SMTP password cannot be decrypted with the current master key")
		}
		pw = string(plain)
	}
	tls, err := mailer.ParseTLSPolicy(s.TLSMode)
	if err != nil {
		return mailer.Config{}, err
	}
	cfg := mailer.Config{Host: s.Host, Port: int(s.Port), Username: s.Username, Password: pw,
		From: mailer.Address{Name: s.FromName, Email: s.FromAddress}, TLSPolicy: tls}
	if tls != mailer.TLSNone {
		cfg.InsecureSkipVerify = s.TLSSkipVerify
		if strings.TrimSpace(s.CACertificate) != "" {
			if cfg.RootCAs, err = mailer.RootsWithPEM(s.CACertificate); err != nil {
				return mailer.Config{}, err
			}
		}
	}
	return cfg, nil
}

// messageID returns a stable RFC 5322 msg-id for retries of one delivery.
func messageID(local, from string) string {
	domain := "localhost"
	if i := strings.LastIndexByte(from, '@'); i >= 0 && i < len(from)-1 {
		domain = from[i+1:]
	}
	return local + "@" + domain
}

func (e *Email) buildTest(_ context.Context, j jobs.Job, d Delivery, snap *settings.Snapshot) (mailer.Message, error) {
	to := j.Payload["to"]
	r, err := e.d.Renderer.Render(templates.TestEmailData{Common: templates.Common{ProductName: snap.Product().Name},
		RequestedBy: j.Payload["requestedBy"], SentAt: e.d.Now()})
	if err != nil {
		return mailer.Message{}, err
	}
	return mailer.Message{To: mailer.Address{Email: to}, Subject: r.Subject, Text: r.Text, HTML: r.HTML,
		MessageId: messageID("test."+d.Id, snap.SMTP().FromAddress),
		Headers:   map[string]string{"X-Open-Kase-Template": r.TemplateId}}, nil
}

var transitionNames = map[string]string{
	outbox.TransitionResolve: templates.TransitionResolved,
	outbox.TransitionReopen:  templates.TransitionReopened,
	outbox.TransitionArchive: templates.TransitionArchived,
}

func (e *Email) buildNotification(ctx context.Context, j jobs.Job, d Delivery, snap *settings.Snapshot) (mailer.Message, error) {
	ev, err := e.d.Events.GetEvent(ctx, j.Payload["eventId"])
	if errors.Is(err, ErrNotFound) {
		return mailer.Message{}, skipped("eventMissing")
	}
	if err != nil {
		return mailer.Message{}, err
	}
	t, err := e.d.Tickets.SystemTicket(ctx, ev.ProjectId, ev.TicketId)
	if denied(err) {
		return mailer.Message{}, skipped("ticketUnavailable") // deleted meanwhile
	}
	if err != nil {
		return mailer.Message{}, err
	}
	tpl := templates.Kind(j.Payload["template"])
	reason := j.Payload["reason"]
	product := snap.Product()
	common := templates.Common{ProductName: product.Name}
	ref := templates.Ticket{Ref: t.Ref(), Subject: t.Subject}
	var to mailer.Address
	var scope *access.ProjectScope

	requesterKind := func() bool {
		switch tpl {
		case templates.KindTicketReceived, templates.KindPublicReply, templates.KindStatusChanged, templates.KindFollowupReminder:
			return true
		}
		return false
	}
	recipientKey := j.Payload["recipientKind"] + "-" + j.Payload["recipientId"]
	switch j.Payload["recipientKind"] {
	case RecipientAddress:
		// A CC address of the ticket: exactly the requester's email (same
		// template, no message content), while it is still on the CC list.
		if !requesterKind() {
			return mailer.Message{}, skipped("notAllowedForContact")
		}
		addr := j.Payload["recipientId"]
		if !slices.Contains(t.CCEmails, addr) {
			return mailer.Message{}, skipped("noLongerCc")
		}
		to = mailer.Address{Email: addr}
		ref.URL = snap.Notifications().RequesterTicketURL(t.Number)
		// Message-ID local parts cannot contain "@": use a digest.
		sum := sha256.Sum256([]byte(addr))
		recipientKey = "cc-" + hex.EncodeToString(sum[:8])
	case RecipientContact:
		// Contacts receive only requester-facing kinds, never staff notices.
		if !requesterKind() {
			return mailer.Message{}, skipped("notAllowedForContact")
		}
		if t.Requester.Id != j.Payload["recipientId"] {
			return mailer.Message{}, skipped("noLongerRequester")
		}
		c, err := e.d.People.Contact(ctx, ev.ProjectId, t.Requester.Id)
		if errors.Is(err, ErrNotFound) {
			return mailer.Message{}, skipped("contactMissing")
		}
		if err != nil {
			return mailer.Message{}, err
		}
		to = mailer.Address{Name: templates.Line(c.Name), Email: c.Email}
		// Requesters have no login: link only to the system that submitted
		// the ticket (configured), never to the agent UI.
		ref.URL = snap.Notifications().RequesterTicketURL(t.Number)
	case RecipientUser:
		u, err := e.d.People.User(ctx, ev.WorkspaceId, j.Payload["recipientId"])
		if errors.Is(err, ErrNotFound) {
			return mailer.Message{}, skipped("recipientMissing")
		}
		if err != nil {
			return mailer.Message{}, err
		}
		if !u.Active {
			return mailer.Message{}, skipped("recipientInactive")
		}
		if (reason == ReasonWatcher && u.MuteWatcher) || (reason == ReasonGroupQueue && u.MuteQueue) {
			return mailer.Message{}, skipped("mutedByPreference")
		}
		// Re-check access now, not at event time.
		p := access.Principal{Kind: access.KindUser, Id: u.Id, WorkspaceId: u.WorkspaceId, Name: u.Name, Email: u.Email, Admin: u.Admin}
		scope, err = e.d.Access.Scope(ctx, p)
		if denied(err) {
			return mailer.Message{}, skipped("recipientNoAccess")
		}
		if err != nil {
			return mailer.Message{}, err
		}
		if _, err := e.d.Tickets.Get(ctx, scope, t.Id); denied(err) {
			return mailer.Message{}, skipped("recipientNoAccess")
		} else if err != nil {
			return mailer.Message{}, err
		}
		if tpl == templates.KindAgentNoteNotice && !scope.Can(access.CapMessageReadPrivate) {
			return mailer.Message{}, skipped("recipientNoAccess")
		}
		if product.BaseURL == "" {
			return mailer.Message{}, skipped("baseUrlNotConfigured")
		}
		ref.URL = strings.TrimRight(product.BaseURL, "/") + "/tickets/" + strconv.FormatInt(t.Number, 10)
		to = mailer.Address{Name: templates.Line(u.Name), Email: u.Email}
	default:
		return mailer.Message{}, skipped("unknownRecipient")
	}
	common.RecipientName = to.Name

	var data templates.Data
	switch tpl {
	case templates.KindTicketReceived:
		data = templates.TicketReceivedData{Common: common, Ticket: ref}
	case templates.KindFollowupReminder:
		if t.StatusCategory != "waitingCustomer" {
			return mailer.Message{}, skipped("noLongerWaiting") // the requester replied meanwhile
		}
		data = templates.FollowupReminderData{Common: common, Ticket: ref, AutoResolveNotice: slices.Contains(ev.Changes, tickets.ReminderMarkAutoResolve)}
	case templates.KindTicketCreated:
		data = templates.TicketCreatedData{Common: common, Ticket: ref, GroupName: t.Group.Name, RequesterName: t.Requester.Name, Priority: t.Priority}
	case templates.KindPublicReply:
		var m tickets.Message
		if scope != nil {
			_, m, err = e.d.Tickets.GetMessage(ctx, scope, t.Id, ev.MessageId)
		} else {
			m, err = e.d.Tickets.SystemMessage(ctx, t.Id, ev.MessageId)
		}
		if denied(err) {
			return mailer.Message{}, skipped("messageUnavailable")
		}
		if err != nil {
			return mailer.Message{}, err
		}
		if m.Visibility != tickets.VisibilityPublic {
			return mailer.Message{}, skipped("notPublic")
		}
		if scope == nil {
			// Requester contact or CC address: a notice without the message content.
			data = templates.RequesterReplyData{Common: common, Ticket: ref}
			break
		}
		data = templates.PublicReplyData{Common: common, Ticket: ref, AuthorName: m.Author.Name, Body: m.Body}
	case templates.KindAgentNoteNotice:
		// Link-only notice: the note body is never loaded.
		author := ""
		switch ev.MessageAuthorKind {
		case RecipientUser:
			if u, err := e.d.People.User(ctx, ev.WorkspaceId, ev.MessageAuthorId); err == nil {
				author = u.Name
			}
		case tickets.AuthorIntegration:
			// An agent token (bot): its name at the time of the note.
			author = ev.Actor.Label + " (bot)"
		}
		data = templates.AgentNoteNoticeData{Common: common, Ticket: ref, AuthorName: author}
	case templates.KindAssignment:
		assignee := ""
		if id := ev.Snapshot.AssigneeId; id != "" {
			if u, err := e.d.People.User(ctx, ev.WorkspaceId, id); err == nil {
				assignee = u.Name
			}
		}
		changedBy := ev.Actor.Label
		if ev.Actor.Type == "user" {
			if u, err := e.d.People.User(ctx, ev.WorkspaceId, ev.Actor.Id); err == nil {
				changedBy = u.Name
			}
		}
		data = templates.AssignmentData{Common: common, Ticket: ref, GroupName: t.Group.Name, AssigneeName: assignee, ChangedByName: changedBy}
	case templates.KindSLAAlert:
		if ev.SLAKind == "" || ev.SLADueAt == nil {
			return mailer.Message{}, skipped("slaDetailsMissing")
		}
		assignee := ""
		if t.Assignee != nil {
			assignee = t.Assignee.Name
		}
		data = templates.SLAAlertData{Common: common, Ticket: ref, Target: ev.SLAKind, Breached: ev.Type == outbox.TicketSLABreached,
			DueAt: *ev.SLADueAt, Priority: t.Priority, GroupName: t.Group.Name, AssigneeName: assignee}
	case templates.KindScheduledReview:
		if len(ev.Changes) == 0 {
			return mailer.Message{}, skipped("reasonMissing")
		}
		vis := "public"
		if slices.Contains(ev.Changes, "visibility:agent") {
			vis = "agent"
		}
		data = templates.ScheduledReviewData{Common: common, Ticket: ref, Reason: ev.Changes[0], Visibility: vis}
	case templates.KindScheduledCancelled:
		if len(ev.Changes) == 0 || !templates.ScheduledCancelNotified(ev.Changes[0]) {
			return mailer.Message{}, skipped("reasonNotNotified")
		}
		vis := "public"
		if slices.Contains(ev.Changes, "visibility:agent") {
			vis = "agent"
		}
		data = templates.ScheduledCancelledData{Common: common, Ticket: ref, Reason: ev.Changes[0], Visibility: vis}
	case templates.KindHoldEnded:
		reason := ""
		for _, c := range ev.Changes {
			if r, ok := strings.CutPrefix(c, tickets.HoldReasonPrefix); ok {
				reason = r
			}
		}
		if reason == "" {
			return mailer.Message{}, skipped("holdReasonMissing")
		}
		data = templates.HoldEndedData{Common: common, Ticket: ref, Reason: reason}
	case templates.KindStatusChanged:
		tr, ok := transitionNames[ev.Transition]
		if !ok {
			return mailer.Message{}, skipped("transitionNotNotified")
		}
		data = templates.StatusChangedData{Common: common, Ticket: ref, Transition: tr, StatusName: t.Status.Name}
	default:
		return mailer.Message{}, fmt.Errorf("%w: unknown template %q", templates.ErrInvalidData, tpl)
	}
	r, err := e.d.Renderer.Render(data)
	if err != nil {
		return mailer.Message{}, err
	}
	return mailer.Message{To: to, Subject: r.Subject, Text: r.Text, HTML: r.HTML,
		MessageId: messageID(ev.Id+"."+recipientKey, snap.SMTP().FromAddress),
		Headers: map[string]string{"X-Open-Kase-Event-Id": ev.Id, "X-Open-Kase-Template": r.TemplateId,
			"X-Open-Kase-Ticket": strconv.FormatInt(t.Number, 10)}}, nil
}
