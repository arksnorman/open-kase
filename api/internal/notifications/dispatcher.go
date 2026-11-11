package notifications

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
)

// ProjectInfo is the project data notifications need.
type ProjectInfo struct {
	Id           string
	RequesterAck bool
}

// EndpointRef is an active webhook endpoint's subscription.
type EndpointRef struct {
	Id                string
	EventTypes        []string
	IncludeAgentNotes bool
}

// Directory reads the facts used for recipient planning.
type Directory interface {
	ProjectInfo(ctx context.Context, projectID string) (ProjectInfo, error)
	Watchers(ctx context.Context, ticketID string) ([]string, error)
	GroupMembers(ctx context.Context, groupID string) ([]string, error)
	// TicketCC returns the ticket's current CC addresses.
	TicketCC(ctx context.Context, ticketID string) ([]string, error)
	// Contact returns a contact's address (ErrNotFound when missing).
	Contact(ctx context.Context, projectID, id string) (ContactInfo, error)
	ActiveEndpoints(ctx context.Context, projectID string) ([]EndpointRef, error)
}

// Dispatcher fans pending outbox events out into jobs.
type Dispatcher struct {
	events   Events
	dir      Directory
	owner    string
	log      *slog.Logger
	now      func() time.Time
	waker    jobs.Waker
	leaseTTL time.Duration
	poll     time.Duration
}

// DispatcherConfig wires a Dispatcher.
type DispatcherConfig struct {
	Events Events
	Dir    Directory
	Owner  string
	Log    *slog.Logger
	Now    func() time.Time
	Waker  jobs.Waker
	Poll   time.Duration
}

// NewDispatcher returns a dispatcher.
func NewDispatcher(c DispatcherConfig) *Dispatcher {
	d := &Dispatcher{events: c.Events, dir: c.Dir, owner: c.Owner, log: c.Log, now: c.Now, waker: c.Waker, leaseTTL: time.Minute, poll: c.Poll}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	if d.now == nil {
		d.now = func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
	}
	if d.poll <= 0 {
		d.poll = time.Second
	}
	return d
}

// maxEventAttempts bounds planning retries before an event is marked failed.
const maxEventAttempts = 10

// Run dispatches events until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		worked, err := d.DispatchOnce(ctx)
		if err != nil && ctx.Err() == nil {
			d.log.Warn("outbox dispatch failed", "error", err.Error())
		}
		if worked {
			t.Reset(0)
		} else {
			t.Reset(d.poll)
		}
	}
}

// DispatchOnce claims and dispatches one pending event.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (bool, error) {
	ev, ok, err := d.events.ClaimEvent(ctx, d.owner, d.now(), d.leaseTTL)
	if err != nil || !ok {
		return false, err
	}
	if err := d.Dispatch(ctx, ev); err != nil {
		retry := d.now().Add(time.Duration(min(1<<min(ev.LeaseGeneration, 10), 600)) * time.Second)
		if ferr := d.events.FailEvent(context.WithoutCancel(ctx), ev, err.Error(), retry, maxEventAttempts); ferr != nil {
			d.log.Warn("could not record outbox dispatch failure", "eventId", ev.Id, "error", ferr.Error())
		}
		return true, err
	}
	return true, nil
}

// Dispatch plans and fans out one event. It is idempotent: jobs are keyed
// by (event, channel, recipient/endpoint), so repeating it creates nothing
// new.
func (d *Dispatcher) Dispatch(ctx context.Context, ev outbox.Record) error {
	planned, err := d.Plan(ctx, ev)
	if err != nil {
		return err
	}
	n, err := d.events.FanOut(ctx, ev, planned, d.now())
	if err != nil {
		return err
	}
	if n > 0 && d.waker != nil {
		d.waker.Notify(ctx)
	}
	if n > 0 {
		d.log.Debug("outbox event dispatched", "eventId", ev.Id, "type", ev.Type, "jobs", n)
	}
	return nil
}

// Plan returns the jobs an event produces.
func (d *Dispatcher) Plan(ctx context.Context, ev outbox.Record) ([]Planned, error) {
	switch ev.Type {
	case outbox.UserDisabled, outbox.GroupMembershipRemoved:
		payload := map[string]string{"eventId": ev.Id, "userId": ev.UserId, "projectId": ev.ProjectId}
		return []Planned{
			{Job: jobs.NewJob{WorkspaceId: ev.WorkspaceId, ProjectId: ev.ProjectId, Kind: KindReleaseAssignment,
				Key: "release:" + ev.Id, Payload: payload}},
		}, nil
	}
	if ev.TicketId == "" || ev.ProjectId == "" {
		return nil, nil
	}
	proj, err := d.dir.ProjectInfo(ctx, ev.ProjectId)
	if err != nil {
		return nil, err
	}
	var out []Planned

	// Email.
	if ev.Snapshot.RequesterId != "" {
		f := Facts{Event: ev, RequesterAck: proj.RequesterAck}
		if f.Watchers, err = d.dir.Watchers(ctx, ev.TicketId); err != nil {
			return nil, err
		}
		if f.CC, err = d.dir.TicketCC(ctx, ev.TicketId); err != nil {
			return nil, err
		}
		if id := ActingContactId(ev); id != "" {
			c, err := d.dir.Contact(ctx, ev.ProjectId, id)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			f.ActorEmail = c.Email
		}
		if ev.Snapshot.GroupId != "" && needsGroup(ev) {
			if f.GroupMembers, err = d.dir.GroupMembers(ctx, ev.Snapshot.GroupId); err != nil {
				return nil, err
			}
		}
		for _, t := range PlanEmail(f) {
			rcpt := Recipient{Kind: t.Kind, Id: t.Id}
			if t.Kind == RecipientAddress {
				rcpt = Recipient{Kind: t.Kind, Address: t.Id} // a CC address has no ID
			}
			out = append(out, Planned{
				Job: jobs.NewJob{WorkspaceId: ev.WorkspaceId, ProjectId: ev.ProjectId, Kind: KindEmail,
					Key: "email:" + ev.Id + ":" + t.Kind + ":" + t.Id,
					Payload: map[string]string{"eventId": ev.Id, "recipientKind": t.Kind, "recipientId": t.Id,
						"reason": t.Reason, "template": string(t.Template)}},
				Delivery: &Delivery{WorkspaceId: ev.WorkspaceId, ProjectId: ev.ProjectId, TicketId: ev.TicketId, EventId: ev.Id,
					EventType: ev.Type, Channel: ChannelEmail, Recipient: rcpt, Reason: t.Reason,
					Template: string(t.Template), Status: StatusPending},
			})
		}
	}

	// Webhooks.
	if typ := webhooks.EventTypeFor(ev.Type, ev.Visibility); typ != "" {
		eps, err := d.dir.ActiveEndpoints(ctx, ev.ProjectId)
		if err != nil {
			return nil, err
		}
		for _, ep := range eps {
			if !slices.Contains(ep.EventTypes, typ) {
				continue
			}
			if typ == webhooks.TypeMessageAgentNote && !ep.IncludeAgentNotes {
				continue // agent-note events need the explicit grant
			}
			out = append(out, Planned{
				Job: jobs.NewJob{WorkspaceId: ev.WorkspaceId, ProjectId: ev.ProjectId, Kind: KindWebhook,
					Key:     "webhook:" + ev.Id + ":" + ep.Id,
					Payload: map[string]string{"eventId": ev.Id, "endpointId": ep.Id, "eventType": typ}},
				Delivery: &Delivery{WorkspaceId: ev.WorkspaceId, ProjectId: ev.ProjectId, TicketId: ev.TicketId, EventId: ev.Id,
					EventType: typ, Channel: ChannelWebhook, Recipient: Recipient{Kind: RecipientEndpoint, Id: ep.Id},
					Reason: ReasonEndpoint, EndpointId: ep.Id, Status: StatusPending},
			})
		}
	}
	return out, nil
}

// needsGroup reports whether planning may address the group queue.
func needsGroup(ev outbox.Record) bool {
	switch ev.Type {
	case outbox.TicketCreated, outbox.TicketReleased, outbox.TicketAssigned, outbox.TicketSLAWarning, outbox.TicketSLABreached:
		return true
	case outbox.MessageCreated:
		return ev.Visibility == "public" && ev.MessageAuthorKind == RecipientContact && ev.Snapshot.AssigneeId == ""
	}
	return false
}
