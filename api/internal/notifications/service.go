package notifications

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Service exposes delivery history, test emails and manual retries.
type Service struct {
	deliveries Deliveries
	jobs       jobs.Store
	waker      jobs.Waker
	audit      AuditWriter
	now        func() time.Time
}

// AuditWriter appends standalone audit events.
type AuditWriter interface {
	Append(ctx context.Context, ev audit.Event) error
}

// NewService returns the notifications service.
func NewService(d Deliveries, js jobs.Store, waker jobs.Waker, aw AuditWriter, now func() time.Time) *Service {
	return &Service{deliveries: d, jobs: js, waker: waker, audit: aw, now: now}
}

func errDeliveryNotFound() error { return fault.NotFound("deliveryNotFound") }

// uniqueKey returns a random component for job keys of one-off sends.
func uniqueKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SendTestEmail enqueues a test email to the given address and returns its
// delivery record (administrators).
func (s *Service) SendTestEmail(ctx context.Context, p access.Principal, to string) (Delivery, error) {
	if err := p.RequireAdmin(); err != nil {
		return Delivery{}, err
	}
	var v validation.Collector
	v.String(validation.NotificationTestTo, "to", to)
	if err := v.Err(); err != nil {
		return Delivery{}, err
	}
	key := uniqueKey()
	requestedBy := p.Name
	if requestedBy == "" {
		requestedBy = p.Email
	}
	d, err := s.deliveries.CreateDirect(ctx, Planned{
		Job: jobs.NewJob{WorkspaceId: p.WorkspaceId, Kind: KindTestEmail, Key: "test-email:" + key,
			Payload: map[string]string{"to": to, "requestedBy": requestedBy}},
		Delivery: &Delivery{WorkspaceId: p.WorkspaceId, Channel: ChannelEmail, Recipient: Recipient{Kind: RecipientAddress, Address: to},
			Reason: ReasonTest, Template: "testEmail", EventType: "email.test", Status: StatusPending},
	})
	if err != nil {
		return Delivery{}, err
	}
	now := s.now()
	ev := audit.New(ctx, p.AuditActor(), audit.NotificationTestEmailRequested, "delivery/"+d.Id, now)
	ev.WorkspaceId = p.WorkspaceId
	if s.audit != nil {
		_ = s.audit.Append(ctx, ev)
	}
	if s.waker != nil {
		s.waker.Notify(ctx)
	}
	return d, nil
}

// GetDelivery returns one delivery (administrators).
func (s *Service) GetDelivery(ctx context.Context, p access.Principal, id string) (Delivery, error) {
	if err := p.RequireAdmin(); err != nil {
		return Delivery{}, err
	}
	if !validation.ValidID(id) {
		return Delivery{}, errDeliveryNotFound()
	}
	d, err := s.deliveries.GetDelivery(ctx, id)
	if errors.Is(err, ErrNotFound) || (err == nil && d.WorkspaceId != p.WorkspaceId) {
		return Delivery{}, errDeliveryNotFound()
	}
	if err != nil {
		return Delivery{}, err
	}
	d.Body = nil
	return s.describe(ctx, d)
}

// describe fills the display details of one delivery's recipient.
func (s *Service) describe(ctx context.Context, d Delivery) (Delivery, error) {
	ds := []Delivery{d}
	if err := s.deliveries.DescribeRecipients(ctx, ds); err != nil {
		return Delivery{}, err
	}
	return ds[0], nil
}

// ListInput filters a delivery listing.
type ListInput struct {
	Channel string
	Status  string
	EventId string
	After   *pagination.Cursor
	Limit   int
}

// ListDeliveries pages through deliveries of the installation, newest
// first (administrators).
func (s *Service) ListDeliveries(ctx context.Context, p access.Principal, in ListInput) (pagination.Page[Delivery], error) {
	if err := p.RequireAdmin(); err != nil {
		return pagination.Page[Delivery]{}, err
	}
	f, err := filterOf(p.WorkspaceId, in)
	if err != nil {
		return pagination.Page[Delivery]{}, err
	}
	return s.list(ctx, f, in.Limit)
}

func filterOf(workspaceID string, in ListInput) (DeliveryFilter, error) {
	var v validation.Collector
	if in.Channel != "" {
		v.String(validation.DeliveryChannel, "channel", in.Channel)
	}
	if in.Status != "" {
		v.String(validation.DeliveryStatus, "status", in.Status)
	}
	if in.EventId != "" {
		v.String(validation.ObjectID, "eventId", in.EventId)
	}
	if err := v.Err(); err != nil {
		return DeliveryFilter{}, err
	}
	f := DeliveryFilter{WorkspaceId: workspaceID, Channel: in.Channel, Status: in.Status, EventId: in.EventId}
	if in.After != nil {
		if !validation.ValidID(in.After.Id) {
			return DeliveryFilter{}, fault.Validation(fault.FieldError{Field: "cursor", Code: "invalidCursor", Message: "The page cursor is invalid or expired."})
		}
		f.BeforeId = in.After.Id
	}
	return f, nil
}

func (s *Service) list(ctx context.Context, f DeliveryFilter, limit int) (pagination.Page[Delivery], error) {
	f.Limit = limit + 1
	rows, err := s.deliveries.ListDeliveries(ctx, f)
	if err != nil {
		return pagination.Page[Delivery]{}, err
	}
	if err := s.deliveries.DescribeRecipients(ctx, rows); err != nil {
		return pagination.Page[Delivery]{}, err
	}
	return pagination.NewPage(rows, limit, func(d Delivery) pagination.Cursor { return pagination.Cursor{Id: d.Id} }), nil
}

// ListForEndpoint pages through one webhook endpoint's deliveries; the
// caller has already authorized the project.
func (s *Service) ListForEndpoint(ctx context.Context, workspaceID, projectID, endpointID string, in ListInput) (pagination.Page[Delivery], error) {
	f, err := filterOf(workspaceID, in)
	if err != nil {
		return pagination.Page[Delivery]{}, err
	}
	f.ProjectId, f.EndpointId, f.Channel = projectID, endpointID, ChannelWebhook
	return s.list(ctx, f, in.Limit)
}

// RetryDelivery requeues a failed (dead-lettered) or unknown-outcome
// delivery with a fresh attempt budget, keeping its event identity
// (administrators).
func (s *Service) RetryDelivery(ctx context.Context, p access.Principal, id string) (Delivery, error) {
	if _, err := s.GetDelivery(ctx, p, id); err != nil {
		return Delivery{}, err
	}
	return s.Requeue(ctx, p.AuditActor(), p.WorkspaceId, id)
}

// Requeue requeues a delivery's job without authorization (callers check).
func (s *Service) Requeue(ctx context.Context, actor audit.Actor, workspaceID, id string) (Delivery, error) {
	d, err := s.deliveries.GetDelivery(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Delivery{}, errDeliveryNotFound()
	}
	if err != nil {
		return Delivery{}, err
	}
	if d.Status != StatusFailed && d.Status != StatusUnknownOutcome {
		// A delivery that is not final is retryable only when its job is
		// dead-lettered all the same (a record left behind by a job that
		// died without an attempt result).
		j, err := s.jobs.Get(ctx, id)
		if err != nil && !errors.Is(err, jobs.ErrNotFound) {
			return Delivery{}, err
		}
		if err != nil || j.State != jobs.StateDead || Terminal(d.Status) {
			return Delivery{}, fault.Conflict("deliveryNotRetryable", "Only failed (dead-lettered) or unknown-outcome deliveries can be retried.")
		}
	}
	now := s.now()
	_, err = s.jobs.Requeue(ctx, id, now, func(ctx context.Context, _ jobs.Job) error {
		return s.deliveries.MarkRequeued(ctx, id, now)
	})
	if errors.Is(err, jobs.ErrNotRetryable) {
		return Delivery{}, fault.Conflict("deliveryNotRetryable", "The delivery is still being processed.")
	}
	if errors.Is(err, jobs.ErrNotFound) {
		return Delivery{}, errDeliveryNotFound()
	}
	if err != nil {
		return Delivery{}, err
	}
	if s.audit != nil {
		ev := audit.New(ctx, actor, audit.NotificationDeliveryRetried, "delivery/"+id, now)
		ev.WorkspaceId, ev.ProjectId = d.WorkspaceId, d.ProjectId
		_ = s.audit.Append(ctx, ev)
	}
	if s.waker != nil {
		s.waker.Notify(ctx)
	}
	out, err := s.deliveries.GetDelivery(ctx, id)
	if err != nil {
		return Delivery{}, err
	}
	out.Body = nil
	return s.describe(ctx, out)
}

// CreateDirect stores one job and its delivery (callers authorize).
func (s *Service) CreateDirect(ctx context.Context, p Planned) (Delivery, error) {
	d, err := s.deliveries.CreateDirect(ctx, p)
	if err != nil {
		return Delivery{}, err
	}
	return s.describe(ctx, d)
}

// LoadDelivery returns a delivery without authorization (callers check).
func (s *Service) LoadDelivery(ctx context.Context, id string) (Delivery, error) {
	d, err := s.deliveries.GetDelivery(ctx, id)
	if err != nil {
		return Delivery{}, err
	}
	return s.describe(ctx, d)
}

// Releaser releases assignments of users who lost eligibility.
type Releaser interface {
	ReleaseIneligible(ctx context.Context, workspaceID, projectID, userID string, actor audit.Actor) (int, error)
}

// ReleaseHandler is the jobs.Handler for KindReleaseAssignment.
func ReleaseHandler(r Releaser) jobs.Handler {
	return func(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
		n, err := r.ReleaseIneligible(ctx, j.WorkspaceId, j.Payload["projectId"], j.Payload["userId"],
			audit.System(audit.SystemJobs, "assignment release job "+j.Id))
		if err != nil {
			return jobs.Result{Outcome: jobs.Retry, ErrorClass: "releaseFailed", Error: err.Error()}
		}
		_ = n
		return jobs.Result{Outcome: jobs.Done}
	}
}
