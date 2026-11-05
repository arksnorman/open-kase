package endpoints

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Keys seals and opens endpoint credentials with the master key.
type Keys interface {
	Seal(plaintext []byte, context string) (string, error)
	Open(ciphertext, context string) ([]byte, error)
}

// PrincipalScopes are the read-only public scopes of a webhook principal.
var PrincipalScopes = []string{access.ScopeTicketsRead}

// Deliveries is the delivery history the service exposes.
type Deliveries interface {
	CreateDirect(ctx context.Context, p notifications.Planned) (notifications.Delivery, error)
	LoadDelivery(ctx context.Context, id string) (notifications.Delivery, error)
	ListForEndpoint(ctx context.Context, workspaceID, projectID, endpointID string, in notifications.ListInput) (pagination.Page[notifications.Delivery], error)
	Requeue(ctx context.Context, actor audit.Actor, workspaceID, id string) (notifications.Delivery, error)
}

// Service manages webhook endpoints.
type Service struct {
	repo       Repository
	keys       Keys
	deliveries Deliveries
	policy     func() webhooks.Policy
	waker      jobs.Waker
	clock      clock.Clock
}

// Deps wires the service.
type Deps struct {
	Repo       Repository
	Keys       Keys
	Deliveries Deliveries
	Policy     func() webhooks.Policy
	Waker      jobs.Waker
	Clock      clock.Clock
}

// NewService returns the endpoint service.
func NewService(d Deps) *Service {
	return &Service{repo: d.Repo, keys: d.Keys, deliveries: d.Deliveries, policy: d.Policy, waker: d.Waker, clock: d.Clock}
}

func errNotFound() error { return fault.NotFound("webhookNotFound") }

func (s *Service) authorize(scope *access.ProjectScope) error {
	if err := scope.Principal.RequireUser(); err != nil {
		return err
	}
	return scope.Require(access.CapIntegrationManage)
}

// Input creates or updates an endpoint; nil fields are unchanged on update.
type Input struct {
	Name              *string
	URL               *string
	EventTypes        *[]string
	Active            *bool
	IncludeAgentNotes *bool
	// Headers replaces the extra headers (see HeaderInput for kept values).
	Headers *[]HeaderInput
	// Auth replaces the authentication (see AuthInput for kept secrets).
	Auth *AuthInput
	// TLS changes the certificate settings.
	TLS *TLSInput
}

func (s *Service) validate(v *validation.Collector, in Input, create bool) {
	if create && in.Name == nil {
		v.Add("name", validation.CodeRequired, "Enter a name for the webhook.")
	}
	if in.Name != nil {
		v.String(validation.WebhookName, "name", *in.Name)
	}
	if create && in.URL == nil {
		v.Add("url", validation.CodeRequired, "Enter the endpoint URL.")
	}
	if in.URL != nil {
		v.String(validation.WebhookURL, "url", *in.URL)
		if validation.WebhookURL.CheckString("url", *in.URL) == nil {
			if _, err := s.policy().CheckURL(*in.URL); err != nil {
				v.Add("url", "destinationBlocked", destinationMessage(err))
			}
		}
	}
	if create && in.EventTypes == nil {
		v.Add("eventTypes", validation.CodeRequired, "Choose at least one event type.")
	}
	if in.EventTypes != nil {
		if len(*in.EventTypes) == 0 {
			v.Add("eventTypes", validation.CodeRequired, "Choose at least one event type.")
		}
		for i, t := range *in.EventTypes {
			v.String(validation.WebhookEventType, "eventTypes["+strconv.Itoa(i)+"]", t)
		}
	}
}

func destinationMessage(err error) string {
	msg := err.Error()
	if errors.Is(err, webhooks.ErrDestinationBlocked) {
		return "This destination is not allowed: " + msg + ". Use an HTTPS URL on a public or private network address."
	}
	return msg
}

func normalizeTypes(ts []string) []string {
	out := slices.Clone(ts)
	slices.Sort(out)
	return slices.Compact(out)
}

// Create registers an endpoint.
func (s *Service) Create(ctx context.Context, scope *access.ProjectScope, in Input) (Endpoint, error) {
	if err := s.authorize(scope); err != nil {
		return Endpoint{}, err
	}
	var v validation.Collector
	s.validate(&v, in, true)
	p := scope.Principal
	now := s.clock.Now()
	e := Endpoint{Id: s.repo.NewID(), WorkspaceId: p.WorkspaceId, ProjectId: scope.Project.Id, Auth: Auth{Type: AuthNone},
		Headers: []Header{}, CreatedBy: p.Id, CreatedAt: now, UpdatedAt: now, Version: 1}
	reqChanges, err := s.applyRequest(&e, in, &v)
	if err != nil {
		return Endpoint{}, err
	}
	agentNotes := in.IncludeAgentNotes != nil && *in.IncludeAgentNotes
	types := normalizeTypes(*in.EventTypes)
	if slices.Contains(types, webhooks.TypeMessageAgentNote) && !agentNotes {
		return Endpoint{}, fault.Field("eventTypes", "agentNotesNotGranted", "ticket.message.agentNote requires includeAgentNotes.")
	}
	e.Name, e.URL, e.EventTypes, e.Active, e.IncludeAgentNotes = strings.TrimSpace(*in.Name), *in.URL, types, in.Active == nil || *in.Active, agentNotes
	if !e.Active {
		e.PausedAt = &now
	}
	ev := audit.New(ctx, p.AuditActor(), audit.WebhookCreated, "", now)
	ev.ProjectId, ev.Changes = scope.Project.Id, append([]string{"name", "url", "eventTypes", "active", "includeAgentNotes"}, reqChanges...)
	return s.repo.Create(ctx, e, principalName(e.Name, *in.URL), ev)
}

func principalName(label, raw string) string {
	name := "Webhook " + label
	if label == "" {
		name = "Webhook"
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			name = "Webhook " + u.Host
		}
	}
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	return name
}

// List pages through the project's endpoints.
func (s *Service) List(ctx context.Context, scope *access.ProjectScope, after *pagination.Cursor, limit int) (pagination.Page[Endpoint], error) {
	if err := s.authorize(scope); err != nil {
		return pagination.Page[Endpoint]{}, err
	}
	rows, err := s.repo.List(ctx, scope.Project.Id, after, limit+1)
	if err != nil {
		return pagination.Page[Endpoint]{}, err
	}
	return pagination.NewPage(rows, limit, func(e Endpoint) pagination.Cursor { return pagination.Cursor{Id: e.Id} }), nil
}

// Stats returns the last-24-hours delivery statistics of the given endpoints
// (one query for all of them). Endpoints without deliveries are absent.
func (s *Service) Stats(ctx context.Context, scope *access.ProjectScope, es []Endpoint) (map[string]Stats, error) {
	if err := s.authorize(scope); err != nil {
		return nil, err
	}
	if len(es) == 0 {
		return map[string]Stats{}, nil
	}
	ids := make([]string, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.Id)
	}
	return s.repo.Stats(ctx, scope.Project.Id, ids, s.clock.Now().Add(-StatsWindow))
}

// Get returns one endpoint.
func (s *Service) Get(ctx context.Context, scope *access.ProjectScope, id string) (Endpoint, error) {
	if err := s.authorize(scope); err != nil {
		return Endpoint{}, err
	}
	if !validation.ValidID(id) {
		return Endpoint{}, errNotFound()
	}
	e, err := s.repo.Get(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) {
		return Endpoint{}, errNotFound()
	}
	return e, err
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return errNotFound()
	case errors.Is(err, ErrVersionConflict):
		return fault.Conflict("versionConflict", "The webhook was changed by someone else. Reload and try again.")
	}
	return err
}

// Update edits url, event types, active, the agent-note grant, headers,
// authentication or TLS settings.
func (s *Service) Update(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64, in Input) (Endpoint, error) {
	e, err := s.Get(ctx, scope, id)
	if err != nil {
		return Endpoint{}, err
	}
	if e.Version != expectedVersion {
		return Endpoint{}, mapErr(ErrVersionConflict)
	}
	var v validation.Collector
	s.validate(&v, in, false)
	orig := e
	changes, err := s.applyRequest(&e, in, &v)
	if err != nil {
		return Endpoint{}, err
	}
	if in.Name != nil {
		if n := strings.TrimSpace(*in.Name); n != e.Name {
			e.Name, changes = n, append(changes, "name")
		}
	}
	if in.URL != nil && *in.URL != e.URL {
		e.URL, changes = *in.URL, append(changes, "url")
	}
	if err := requireCredentialsAgain(orig, e); err != nil {
		return Endpoint{}, err
	}
	if in.EventTypes != nil {
		if t := normalizeTypes(*in.EventTypes); !slices.Equal(t, e.EventTypes) {
			e.EventTypes, changes = t, append(changes, "eventTypes")
		}
	}
	if in.Active != nil && *in.Active != e.Active {
		e.Active, changes = *in.Active, append(changes, "active")
		if e.Active {
			e.PausedAt = nil
		} else {
			t := s.clock.Now()
			e.PausedAt = &t
		}
	}
	if in.IncludeAgentNotes != nil && *in.IncludeAgentNotes != e.IncludeAgentNotes {
		e.IncludeAgentNotes, changes = *in.IncludeAgentNotes, append(changes, "includeAgentNotes")
	}
	if !e.IncludeAgentNotes && slices.Contains(e.EventTypes, webhooks.TypeMessageAgentNote) {
		return Endpoint{}, fault.Field("eventTypes", "agentNotesNotGranted", "ticket.message.agentNote requires includeAgentNotes.")
	}
	if len(changes) == 0 {
		return e, nil
	}
	now := s.clock.Now()
	e.UpdatedAt = now
	ev := audit.New(ctx, scope.AuditActor(), audit.WebhookUpdated, "webhook/"+e.Id, now)
	ev.ProjectId, ev.Changes = scope.Project.Id, changes
	pn := ""
	if slices.Contains(changes, "name") {
		pn = principalName(e.Name, e.URL)
	}
	out, err := s.repo.Update(ctx, e, pn, expectedVersion, ev)
	return out, mapErr(err)
}

// Delete removes an endpoint and revokes its principal. Pending deliveries
// are skipped when they run.
func (s *Service) Delete(ctx context.Context, scope *access.ProjectScope, id string) error {
	if _, err := s.Get(ctx, scope, id); err != nil {
		return err
	}
	now := s.clock.Now()
	ev := audit.New(ctx, scope.AuditActor(), audit.WebhookDeleted, "webhook/"+id, now)
	ev.ProjectId = scope.Project.Id
	return mapErr(s.repo.Delete(ctx, scope.Project.Id, id, now, ev))
}

// Test enqueues a "webhook.test" delivery to the endpoint and
// returns its delivery record. It uses the same destination checks.
func (s *Service) Test(ctx context.Context, scope *access.ProjectScope, id string) (notifications.Delivery, error) {
	e, err := s.Get(ctx, scope, id)
	if err != nil {
		return notifications.Delivery{}, err
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	key := hex.EncodeToString(b)
	d, err := s.deliveries.CreateDirect(ctx, notifications.Planned{
		Job: jobs.NewJob{WorkspaceId: e.WorkspaceId, ProjectId: e.ProjectId, Kind: notifications.KindWebhook, Key: "webhook-test:" + key,
			Payload: map[string]string{"endpointId": e.Id, "eventType": webhooks.TypeTest}},
		Delivery: &notifications.Delivery{WorkspaceId: e.WorkspaceId, ProjectId: e.ProjectId, EventType: webhooks.TypeTest,
			Channel: notifications.ChannelWebhook, Recipient: notifications.Recipient{Kind: notifications.RecipientEndpoint, Id: e.Id},
			Reason: notifications.ReasonTest, EndpointId: e.Id, Status: notifications.StatusPending},
	})
	if err != nil {
		return notifications.Delivery{}, err
	}
	if s.waker != nil {
		s.waker.Notify(ctx)
	}
	return d, nil
}

// Deliveries pages through an endpoint's delivery history.
func (s *Service) Deliveries(ctx context.Context, scope *access.ProjectScope, id string, in notifications.ListInput) (pagination.Page[notifications.Delivery], error) {
	e, err := s.Get(ctx, scope, id)
	if err != nil {
		return pagination.Page[notifications.Delivery]{}, err
	}
	return s.deliveries.ListForEndpoint(ctx, e.WorkspaceId, e.ProjectId, e.Id, in)
}

// Delivery returns one delivery of the endpoint.
func (s *Service) Delivery(ctx context.Context, scope *access.ProjectScope, id, deliveryID string) (notifications.Delivery, error) {
	e, err := s.Get(ctx, scope, id)
	if err != nil {
		return notifications.Delivery{}, err
	}
	if !validation.ValidID(deliveryID) {
		return notifications.Delivery{}, fault.NotFound("deliveryNotFound")
	}
	d, err := s.deliveries.LoadDelivery(ctx, deliveryID)
	if errors.Is(err, notifications.ErrNotFound) || (err == nil && d.EndpointId != e.Id) {
		return notifications.Delivery{}, fault.NotFound("deliveryNotFound")
	}
	d.Body = nil
	return d, err
}

// Retry requeues a dead-lettered delivery of the endpoint. The event ID
// and payload bytes stay the same; permissions and the destination are
// checked again when it runs.
func (s *Service) Retry(ctx context.Context, scope *access.ProjectScope, id, deliveryID string) (notifications.Delivery, error) {
	d, err := s.Delivery(ctx, scope, id, deliveryID)
	if err != nil {
		return notifications.Delivery{}, err
	}
	return s.deliveries.Requeue(ctx, scope.AuditActor(), d.WorkspaceId, d.Id)
}

// requireCredentialsAgain rejects an update that moves the endpoint to a
// different destination while keeping stored secrets: they were entered for the
// old destination and would otherwise be sent to the new one (the same rule as
// the SMTP password). Clearing the authentication never needs re-entry.
func requireCredentialsAgain(old, next Endpoint) error {
	if !destinationChanged(old, next) {
		return nil
	}
	if (next.Auth.Type == AuthBasic && next.Auth.SealedPassword != "" && next.Auth.SealedPassword == old.Auth.SealedPassword) ||
		(next.Auth.Type == AuthToken && next.Auth.SealedToken != "" && next.Auth.SealedToken == old.Auth.SealedToken) {
		return fault.Field("auth", validation.CodeRequired, "Enter the credentials again: the destination changed.")
	}
	for i, h := range next.Headers {
		for _, o := range old.Headers {
			if h.SealedValue != "" && h.SealedValue == o.SealedValue {
				return fault.Field("headers["+strconv.Itoa(i)+"].value", validation.CodeRequired, "Enter the value again: the destination changed.")
			}
		}
	}
	return nil
}

// destinationChanged reports a different scheme, host or port, or a custom CA
// changed to another non-empty value. The client key is not sent to the receiver.
func destinationChanged(a, b Endpoint) bool {
	if strings.TrimSpace(b.TLS.CACertificate) != "" && strings.TrimSpace(a.TLS.CACertificate) != strings.TrimSpace(b.TLS.CACertificate) {
		return true
	}
	if a.URL == b.URL {
		return false
	}
	ua, errA := url.Parse(a.URL)
	ub, errB := url.Parse(b.URL)
	if errA != nil || errB != nil {
		return true
	}
	return asciiLower(ua.Scheme) != asciiLower(ub.Scheme) || asciiLower(ua.Hostname()) != asciiLower(ub.Hostname()) || effectivePort(ua) != effectivePort(ub)
}

// asciiLower lowercases A-Z only, so Unicode case folding cannot make two
// different hosts look equal (anything non-ASCII must match byte for byte).
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if asciiLower(u.Scheme) == "http" {
		return "80"
	}
	return "443"
}
