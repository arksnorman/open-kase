// Package notifications turns durable outbox events into delivery jobs and
// delivers notification email.
//
// The dispatcher reads pending outbox events, computes candidate recipients
// from the event's audience (recipients.go) and creates one uniquely keyed
// job plus one delivery record per recipient or webhook endpoint in a single
// transaction, so running it twice (or concurrently) never duplicates work.
// Email workers re-check each recipient's current access and preferences
// immediately before sending, render a per-recipient authorized view and
// record the outcome (sent, failed, unknownOutcome, skipped,
// skippedUnconfigured) in notificationDeliveries.
package notifications

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
)

// Job kinds.
const (
	KindEmail             = "email.notification"
	KindTestEmail         = "email.test"
	KindWebhook           = "webhook.delivery"
	KindReleaseAssignment = "assignments.release"
)

// Delivery channels.
const (
	ChannelEmail   = "email"
	ChannelWebhook = "webhook"
)

// Delivery statuses. "failed" is the dead-letter status (retries exhausted
// or a permanent error); "unknownOutcome" means the SMTP server may have
// accepted the message before the connection failed, so it is not retried
// automatically to avoid duplicates.
const (
	StatusPending             = "pending"
	StatusRetrying            = "retrying"
	StatusSent                = "sent"
	StatusFailed              = "failed"
	StatusUnknownOutcome      = "unknownOutcome"
	StatusSkipped             = "skipped"
	StatusSkippedUnconfigured = "skippedUnconfigured"
)

// Terminal reports whether a delivery status is final for its job.
func Terminal(status string) bool {
	switch status {
	case StatusSent, StatusFailed, StatusUnknownOutcome, StatusSkipped, StatusSkippedUnconfigured:
		return true
	}
	return false
}

// Recipient kinds.
const (
	RecipientUser     = "user"
	RecipientContact  = "contact"
	RecipientAddress  = "address"  // test email, or a ticket CC address
	RecipientEndpoint = "endpoint" // webhook endpoint
)

// Recipient reasons.
const (
	ReasonRequester = "requester"
	// ReasonCC: a CC address of the ticket, copied on requester emails.
	ReasonCC         = "cc"
	ReasonAssignee   = "assignee"
	ReasonWatcher    = "watcher"
	ReasonGroupQueue = "groupQueue"
	// ReasonAuthor: the author of a scheduled message held for review.
	ReasonAuthor   = "author"
	ReasonTest     = "test"
	ReasonEndpoint = "endpoint"
)

// Recipient identifies who a delivery is for. Address is the email address
// actually used (filled when sending), or the URL host for webhooks.
type Recipient struct {
	Kind    string
	Id      string
	Address string
	// Name, Email and URL are display details looked up when deliveries
	// are listed or read (not stored): the user's or contact's name and
	// current email, or the webhook endpoint's URL.
	Name  string
	Email string
	URL   string
}

// Delivery is the visible history of one job's delivery.
type Delivery struct {
	Id          string // equals the job ID
	WorkspaceId string
	ProjectId   string
	TicketId    string
	// TicketNumber is the number of the ticket, looked up when deliveries are
	// listed or read (not stored); 0 when unknown.
	TicketNumber    int64
	EventId         string
	EventType       string
	Channel         string
	Recipient       Recipient
	Reason          string
	Template        string
	EndpointId      string
	Status          string
	Attempts        int
	NextAttemptAt   *time.Time
	LastAttemptAt   *time.Time
	ErrorClass      string
	Error           string
	StatusCode      int
	ResponseSnippet string
	MessageId       string
	// ResponseMs is how long the receiver took to answer the last attempt (webhooks; 0 when there was no response).
	ResponseMs int
	// Body is the exact webhook payload, stored at the first attempt so
	// retries send identical bytes. Never returned by list views.
	Body []byte
	// BodyGen counts invalidations of the stored payload (a redaction in
	// the ticket, a withdrawn agent-note grant). A payload built while it
	// moved is neither stored nor sent: it may hold the removed content.
	BodyGen     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// DeliveryUpdate records an attempt's result.
type DeliveryUpdate struct {
	Status          string
	Attempts        int
	NextAttemptAt   *time.Time
	LastAttemptAt   time.Time
	ErrorClass      string
	Error           string
	StatusCode      int
	ResponseSnippet string
	// ResponseMs is how long the receiver took to answer (webhooks).
	ResponseMs int
	MessageId  string
	Address    string
}

// Planned is one job (and its delivery record, if it delivers something)
// produced by fanning out an event.
type Planned struct {
	Job      jobs.NewJob
	Delivery *Delivery
}

// DeliveryFilter selects deliveries for listing.
type DeliveryFilter struct {
	WorkspaceId string
	ProjectId   string
	EndpointId  string
	Channel     string
	Status      string
	EventId     string
	BeforeId    string // keyset: newest first
	Limit       int
}

// ErrNotFound is returned for missing deliveries or events.
var ErrNotFound = errors.New("notifications: not found")

// Deliveries persists delivery records.
type Deliveries interface {
	GetDelivery(ctx context.Context, id string) (Delivery, error)
	// UpdateDelivery is called inside a fenced job transaction.
	UpdateDelivery(ctx context.Context, id string, u DeliveryUpdate) error
	ListDeliveries(ctx context.Context, f DeliveryFilter) ([]Delivery, error)
	// CreateDirect stores one job and its delivery atomically (test sends).
	CreateDirect(ctx context.Context, p Planned) (Delivery, error)
	// MarkRequeued resets a delivery for a manual retry (inside the
	// requeue transaction).
	MarkRequeued(ctx context.Context, id string, now time.Time) error
	// MarkDead records that the delivery's job was dead-lettered without
	// an attempt result: a delivery that is not final yet becomes failed
	// (called inside the job's fenced completion transaction).
	MarkDead(ctx context.Context, id string, u DeliveryUpdate) error
	// DescribeRecipients fills the display details of the deliveries'
	// recipients (bounded: one query per recipient kind).
	DescribeRecipients(ctx context.Context, ds []Delivery) error
}

// DeadHook returns the jobs.DeadHook of delivery jobs. The runner
// dead-letters a job without asking its handler when the last attempt's
// lease expired (worker killed, completion failed) or the handler kept
// panicking; without this hook the delivery stayed "pending" or "retrying"
// for ever and could not be retried by an administrator.
func DeadHook(d Deliveries) jobs.DeadHook {
	return func(ctx context.Context, j jobs.Job, f jobs.Finish) error {
		return d.MarkDead(ctx, j.Id, DeliveryUpdate{Status: StatusFailed, Attempts: j.Attempts, LastAttemptAt: f.Attempt.EndedAt,
			ErrorClass: f.Attempt.ErrorClass, Error: f.Attempt.Error})
	}
}

// Events reads and fans out outbox events.
type Events interface {
	// ClaimEvent leases the oldest pending event whose lease is free.
	ClaimEvent(ctx context.Context, owner string, now time.Time, ttl time.Duration) (outbox.Record, bool, error)
	GetEvent(ctx context.Context, id string) (outbox.Record, error)
	// FanOut creates the planned jobs and deliveries that do not exist yet
	// and marks the event dispatched, in one transaction fenced by the
	// event's lease generation (when gen > 0). It returns how many jobs it
	// created.
	FanOut(ctx context.Context, ev outbox.Record, planned []Planned, now time.Time) (int, error)
	// FailEvent releases the lease after a planning error; after
	// maxAttempts the event is marked failed.
	FailEvent(ctx context.Context, ev outbox.Record, msg string, retryAt time.Time, maxAttempts int) error
}
