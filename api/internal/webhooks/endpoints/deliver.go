package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
	"github.com/arksnorman/open-kase/api/package/fault"
)

// DeliveryStore is the delivery persistence the worker needs.
type DeliveryStore interface {
	GetDelivery(ctx context.Context, id string) (notifications.Delivery, error)
	UpdateDelivery(ctx context.Context, id string, u notifications.DeliveryUpdate) error
	// SetDeliveryBody stores the payload unless one is stored already or
	// the body generation is no longer gen.
	SetDeliveryBody(ctx context.Context, id string, body []byte, gen int64, now time.Time) error
}

// errPayloadChanged: a redaction or a withdrawn grant moved the body
// generation while the payload was being built from earlier content.
var errPayloadChanged = errors.New("the ticket content or the endpoint's grant changed while the payload was built; it is rebuilt at the next attempt")

// DelivererDeps wires the webhook worker.
type DelivererDeps struct {
	Jobs       jobs.Store
	Deliveries DeliveryStore
	Events     interface {
		GetEvent(ctx context.Context, id string) (outbox.Record, error)
	}
	Endpoints Repository
	Access    notifications.Authorizer
	Tickets   notifications.Tickets
	Keys      Keys
	// Policy returns the destination policy (HTTP and loopback only in
	// development).
	Policy func() webhooks.Policy
	Now    func() time.Time
	Jitter webhooks.Jitter
}

// Deliverer sends webhook delivery jobs.
type Deliverer struct{ d DelivererDeps }

// NewDeliverer returns the webhook job handler.
func NewDeliverer(d DelivererDeps) *Deliverer {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
	}
	return &Deliverer{d: d}
}

type skip string

func (s skip) Error() string { return "skipped: " + string(s) }

func denied(err error) bool {
	var fe *fault.Error
	return errors.As(err, &fe) && (fe.Kind == fault.KindNotFound || fe.Kind == fault.KindForbidden)
}

// Handle is the jobs.Handler for notifications.KindWebhook.
func (w *Deliverer) Handle(ctx context.Context, j jobs.Job, l jobs.Lease) jobs.Result {
	d, err := w.d.Deliveries.GetDelivery(ctx, j.Id)
	if errors.Is(err, notifications.ErrNotFound) {
		return jobs.Result{Outcome: jobs.Dead, ErrorClass: "deliveryMissing", Error: "no delivery record for this job"}
	}
	if err != nil {
		return jobs.Result{Outcome: jobs.Retry, ErrorClass: "database", Error: err.Error()}
	}
	if notifications.Terminal(d.Status) {
		return jobs.Result{Outcome: jobs.Done}
	}
	now := w.d.Now()
	upd := notifications.DeliveryUpdate{Attempts: j.Attempts, LastAttemptAt: now}
	finish := func(status, class, msg string, outcome jobs.Outcome, delay time.Duration) jobs.Result {
		return jobs.Result{Outcome: outcome, Delay: delay, ErrorClass: class, Error: msg, Commit: func(ctx context.Context, f jobs.Finish) error {
			u := upd
			u.Status, u.ErrorClass, u.Error = status, class, msg
			switch f.State {
			case jobs.StateQueued:
				u.Status, u.NextAttemptAt = notifications.StatusRetrying, &f.RunAt
			case jobs.StateDead:
				u.Status = notifications.StatusFailed
			}
			return w.d.Deliveries.UpdateDelivery(ctx, j.Id, u)
		}}
	}

	ep, body, eventID, err := w.prepare(ctx, j, l, d)
	var sk skip
	switch {
	case errors.As(err, &sk):
		return finish(notifications.StatusSkipped, string(sk), "", jobs.Done, 0)
	case errors.Is(err, jobs.ErrLeaseLost):
		return jobs.Result{Outcome: jobs.Retry, ErrorClass: "leaseLost", Error: err.Error()}
	case err != nil:
		return finish(notifications.StatusRetrying, "payloadFailed", err.Error(), jobs.Retry, 0)
	}
	headers, authorization, tlsCfg, err := requestConfig(w.d.Keys, ep)
	if err != nil {
		return finish(notifications.StatusFailed, string(webhooks.ErrorConfig), err.Error(), jobs.Dead, 0)
	}
	client := webhooks.NewClient(w.d.Policy())
	res := client.Deliver(ctx, webhooks.Request{URL: ep.URL, EventId: eventID, EventType: j.Payload["eventType"], Body: body,
		Headers: headers, Authorization: authorization, TLS: tlsCfg})
	upd.StatusCode, upd.ResponseSnippet = res.StatusCode, res.ResponseSnippet
	if res.StatusCode > 0 {
		upd.ResponseMs = max(1, int(res.Duration.Milliseconds()))
	}
	upd.Address = hostOf(ep.URL)
	// Attempt number within the current budget (a manual retry starts a
	// fresh schedule).
	attempt := j.Attempts - (j.MaxAttempts - jobs.DefaultMaxAttempts)
	dec := webhooks.Decide(attempt, res, w.d.Jitter)
	msg := ""
	if res.Err != nil {
		msg = res.Err.Error()
	}
	switch {
	case dec.Done:
		return finish(notifications.StatusSent, "", "", jobs.Done, 0)
	case dec.DeadLetter:
		return finish(notifications.StatusFailed, string(res.ErrorClass), msg, jobs.Dead, 0)
	default:
		return finish(notifications.StatusRetrying, string(res.ErrorClass), msg, jobs.Retry, dec.Delay)
	}
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

// prepare re-checks the endpoint and its principal and returns the exact
// payload bytes, building and storing them on the first attempt.
func (w *Deliverer) prepare(ctx context.Context, j jobs.Job, l jobs.Lease, d notifications.Delivery) (Endpoint, []byte, string, error) {
	ep, err := w.d.Endpoints.Get(ctx, "", j.Payload["endpointId"])
	if errors.Is(err, ErrNotFound) {
		return Endpoint{}, nil, "", skip("endpointDeleted")
	}
	if err != nil {
		return Endpoint{}, nil, "", err
	}
	if !ep.Active {
		return Endpoint{}, nil, "", skip("endpointInactive")
	}
	if ok, err := w.d.Endpoints.PrincipalActive(ctx, ep.PrincipalId); err != nil {
		return Endpoint{}, nil, "", err
	} else if !ok {
		return Endpoint{}, nil, "", skip("principalRevoked")
	}
	typ := j.Payload["eventType"]
	if typ != webhooks.TypeTest && !slices.Contains(ep.EventTypes, typ) {
		return Endpoint{}, nil, "", skip("eventTypeUnsubscribed")
	}
	if typ == webhooks.TypeMessageAgentNote && !ep.IncludeAgentNotes {
		return Endpoint{}, nil, "", skip("agentNotesNotGranted")
	}
	eventID := j.Payload["eventId"]
	if typ == webhooks.TypeTest {
		eventID = d.Id
	}
	if len(d.Body) > 0 {
		return ep, d.Body, eventID, nil
	}
	// d was read before the payload is built: a redaction or a withdrawn
	// grant committed from here on moves the body generation, and the
	// payload (possibly built from the removed content) is then neither
	// stored nor sent.
	gen := d.BodyGen
	body, err := w.build(ctx, ep, typ, eventID)
	if err != nil {
		return Endpoint{}, nil, "", err
	}
	// Store the bytes before sending so every retry sends identical content.
	if err := w.d.Jobs.Fenced(ctx, l, func(ctx context.Context) error {
		return w.d.Deliveries.SetDeliveryBody(ctx, d.Id, body, gen, w.d.Now())
	}); err != nil {
		return Endpoint{}, nil, "", err
	}
	cur, err := w.d.Deliveries.GetDelivery(ctx, d.Id)
	if err != nil {
		return Endpoint{}, nil, "", err
	}
	if cur.BodyGen != gen || len(cur.Body) == 0 {
		return Endpoint{}, nil, "", errPayloadChanged
	}
	return ep, cur.Body, eventID, nil
}

type refDTO struct {
	Id   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type statusDTO struct {
	Id       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Category string `json:"category"`
}

type requesterDTO struct {
	Id         string  `json:"id"`
	Name       string  `json:"name"`
	Email      string  `json:"email"`
	ExternalId *string `json:"externalId"`
}

// attachmentDTO is a message attachment's metadata (never its bytes). A
// file removed by a redaction is the placeholder {id, removed: true}.
type attachmentDTO struct {
	Id          string `json:"id"`
	Filename    string `json:"filename,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Removed     bool   `json:"removed,omitempty"`
}

type messageDTO struct {
	Id         string `json:"id"`
	Visibility string `json:"visibility"`
	Author     struct {
		Context string `json:"context"`
		Name    string `json:"name"`
	} `json:"author"`
	Body        string          `json:"body"`
	Attachments []attachmentDTO `json:"attachments"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// build creates the envelope from what the endpoint's principal may read.
// Data is built per endpoint from the authorized projection, never by
// trimming a full record.
func (w *Deliverer) build(ctx context.Context, ep Endpoint, typ, eventID string) ([]byte, error) {
	env := webhooks.Envelope{EventId: eventID, Type: typ}
	if typ == webhooks.TypeTest {
		env.OccurredAt = w.d.Now()
		env.Data = json.RawMessage(`{"test":true,"message":"This is a test delivery from open-kase."}`)
		return env.Marshal()
	}
	ev, err := w.d.Events.GetEvent(ctx, eventID)
	if errors.Is(err, notifications.ErrNotFound) {
		return nil, skip("eventMissing")
	}
	if err != nil {
		return nil, err
	}
	env.OccurredAt = ev.OccurredAt
	principal := access.Principal{Kind: access.KindIntegration, Id: ep.PrincipalId, WorkspaceId: ep.WorkspaceId, Name: "webhook",
		ProjectId: ep.ProjectId, Scopes: PrincipalScopes}
	scope, err := w.d.Access.Scope(ctx, principal)
	if denied(err) {
		return nil, skip("noAccess")
	}
	if err != nil {
		return nil, err
	}
	staff := ep.IncludeAgentNotes

	t, err := w.d.Tickets.Get(ctx, scope, ev.TicketId)
	if denied(err) {
		return nil, skip("ticketUnavailable")
	}
	if err != nil {
		return nil, err
	}
	env.Ticket = &webhooks.TicketRef{Id: t.Id, Number: t.Number, Revision: ev.TicketVersion}
	snap := ev.Snapshot
	data := map[string]any{}
	status := statusDTO{Id: snap.StatusId, Category: snap.StatusCategory}
	if t.Status.Id == snap.StatusId {
		status.Name = t.Status.Name
	}
	group := refDTO{Id: snap.GroupId}
	if t.Group.Id == snap.GroupId {
		group.Name = t.Group.Name
	}

	switch typ {
	case webhooks.TypeTicketCreated:
		var ext *string
		if t.Requester.ExternalId != "" {
			ext = &t.Requester.ExternalId
		}
		data["subject"], data["priority"], data["status"], data["group"] = snap.Subject, snap.Priority, status, group
		data["source"] = t.Source
		data["requestType"] = refDTO{Id: t.RequestType.Id, Name: t.RequestType.Name}
		data["requester"] = requesterDTO{Id: t.Requester.Id, Name: t.Requester.Name, Email: t.Requester.Email, ExternalId: ext}
		if data["organisation"], err = w.organisationPayload(ctx, t, snap); err != nil {
			return nil, err
		}
		if t.RequestedPriority > 0 {
			data["requestedPriority"] = t.RequestedPriority
		}
		data["createdAt"] = t.CreatedAt
	case webhooks.TypeTicketUpdated:
		changes := ev.Changes
		if changes == nil {
			changes = []string{}
		}
		data["changes"], data["subject"], data["priority"] = changes, snap.Subject, snap.Priority
		data["requestType"] = refDTO{Id: snap.RequestTypeId}
		if data["organisation"], err = w.organisationPayload(ctx, t, snap); err != nil {
			return nil, err
		}
	case webhooks.TypeMessagePublic, webhooks.TypeMessageAgentNote:
		var m tickets.Message
		if typ == webhooks.TypeMessagePublic {
			_, m, err = w.d.Tickets.GetMessage(ctx, scope, t.Id, ev.MessageId)
		} else {
			if !staff {
				return nil, skip("agentNotesNotGranted")
			}
			m, err = w.d.Tickets.SystemMessage(ctx, t.Id, ev.MessageId)
		}
		if denied(err) {
			return nil, skip("messageUnavailable")
		}
		if err != nil {
			return nil, err
		}
		if (typ == webhooks.TypeMessagePublic) != (m.Visibility == tickets.VisibilityPublic) {
			return nil, skip("audienceMismatch")
		}
		md := messageDTO{Id: m.Id, Visibility: m.Visibility, Body: m.Body, CreatedAt: m.CreatedAt, Attachments: []attachmentDTO{}}
		md.Author.Context, md.Author.Name = m.Author.Context(), m.Author.Name
		for _, a := range m.Attachments {
			if a.Removed() {
				md.Attachments = append(md.Attachments, attachmentDTO{Id: a.Id, Removed: true})
				continue
			}
			md.Attachments = append(md.Attachments, attachmentDTO{Id: a.Id, Filename: a.Filename, Size: a.Size, ContentType: a.ContentType})
		}
		data["message"] = md
	case webhooks.TypeTicketAssigned:
		data["group"] = group
		if staff {
			if snap.AssigneeId == "" {
				data["assignee"] = nil
			} else {
				a := refDTO{Id: snap.AssigneeId}
				if raw, err := w.d.Tickets.SystemTicket(ctx, ev.ProjectId, ev.TicketId); err == nil && raw.Assignee != nil && raw.Assignee.Id == snap.AssigneeId {
					a.Name = raw.Assignee.Name
				}
				data["assignee"] = a
			}
		}
	case webhooks.TypeTicketStatusChanged:
		data["status"], data["transition"] = status, ev.Transition
	case webhooks.TypeSLAWarning, webhooks.TypeSLABreached:
		sl := map[string]any{"kind": ev.SLAKind, "state": "dueSoon"}
		if typ == webhooks.TypeSLABreached {
			sl["state"] = "breached"
		}
		if ev.SLADueAt != nil {
			sl["dueAt"] = ev.SLADueAt.UTC()
		}
		data["sla"], data["priority"] = sl, snap.Priority
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	env.Data = raw
	return env.Marshal()
}

// optString is nil for "" (JSON null) and the value otherwise.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// organisationPayload is the event's organisation (nil, JSON null, when the ticket had
// none), loaded by the ID the event recorded. When it cannot be loaded (for example it
// was deleted since) only the ID is sent: name and externalRef are omitted, not null.
// Any other error is returned so the delivery retries.
func (w *Deliverer) organisationPayload(ctx context.Context, t tickets.Ticket, snap outbox.Snapshot) (map[string]any, error) {
	if snap.OrganisationId == "" {
		return nil, nil
	}
	out := map[string]any{"id": snap.OrganisationId}
	o := tickets.OrgRef{}
	if t.Organisation != nil && t.Organisation.Id == snap.OrganisationId {
		o = *t.Organisation
	} else if got, err := w.d.Tickets.OrganisationRef(ctx, t.ProjectId, snap.OrganisationId); err == nil {
		o = got
	} else if errors.Is(err, tickets.ErrNotFound) {
		return out, nil // deleted since: only the id
	} else {
		return nil, err // transient: the delivery retries
	}
	out["name"], out["externalRef"] = o.Name, optString(o.ExternalRef)
	return out, nil
}
