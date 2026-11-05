package mongo

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/webhooks/endpoints"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

// webhookHeaderDoc is an extra request header; the value is sealed.
type webhookHeaderDoc struct {
	Name            string `bson:"name"`
	ValueCiphertext string `bson:"valueCiphertext"`
}

// webhookAuthDoc is the endpoint's Authorization; secrets are sealed.
type webhookAuthDoc struct {
	Type               string `bson:"type"`
	Username           string `bson:"username,omitempty"`
	PasswordCiphertext string `bson:"passwordCiphertext,omitempty"`
	TokenCiphertext    string `bson:"tokenCiphertext,omitempty"`
}

// webhookTLSDoc holds PEM certificates and the sealed client key.
type webhookTLSDoc struct {
	CACertificate       string `bson:"caCertificate,omitempty"`
	ClientCertificate   string `bson:"clientCertificate,omitempty"`
	ClientKeyCiphertext string `bson:"clientKeyCiphertext,omitempty"`
}

type webhookEndpointDoc struct {
	Id                bson.ObjectID      `bson:"_id"`
	WorkspaceId       bson.ObjectID      `bson:"workspaceId"`
	ProjectId         bson.ObjectID      `bson:"projectId"`
	PrincipalId       bson.ObjectID      `bson:"principalId"`
	Name              string             `bson:"name"`
	URL               string             `bson:"url"`
	EventTypes        []string           `bson:"eventTypes"`
	Active            bool               `bson:"active"`
	IncludeAgentNotes bool               `bson:"includeAgentNotes"`
	Headers           []webhookHeaderDoc `bson:"headers"`
	Auth              webhookAuthDoc     `bson:"auth"`
	TLS               webhookTLSDoc      `bson:"tls"`
	CreatedBy         bson.ObjectID      `bson:"createdBy"`
	CreatedAt         time.Time          `bson:"createdAt"`
	UpdatedAt         time.Time          `bson:"updatedAt"`
	PausedAt          *time.Time         `bson:"pausedAt"`
	Version           int64              `bson:"version"`
}

func (d webhookEndpointDoc) toDomain() endpoints.Endpoint {
	e := endpoints.Endpoint{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), PrincipalId: d.PrincipalId.Hex(),
		Name: d.Name, URL: d.URL, EventTypes: d.EventTypes, Active: d.Active, IncludeAgentNotes: d.IncludeAgentNotes, CreatedBy: d.CreatedBy.Hex(),
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, PausedAt: d.PausedAt, Version: d.Version}
	if e.EventTypes == nil {
		e.EventTypes = []string{}
	}
	e.Headers = make([]endpoints.Header, 0, len(d.Headers))
	for _, h := range d.Headers {
		e.Headers = append(e.Headers, endpoints.Header{Name: h.Name, SealedValue: h.ValueCiphertext})
	}
	e.Auth = endpoints.Auth{Type: d.Auth.Type, Username: d.Auth.Username, SealedPassword: d.Auth.PasswordCiphertext, SealedToken: d.Auth.TokenCiphertext}
	if e.Auth.Type == "" {
		e.Auth.Type = endpoints.AuthNone
	}
	e.TLS = endpoints.TLS{CACertificate: d.TLS.CACertificate, ClientCertificate: d.TLS.ClientCertificate, SealedClientKey: d.TLS.ClientKeyCiphertext}
	return e
}

func webhookHeaderDocs(hs []endpoints.Header) []webhookHeaderDoc {
	out := make([]webhookHeaderDoc, 0, len(hs))
	for _, h := range hs {
		out = append(out, webhookHeaderDoc{Name: h.Name, ValueCiphertext: h.SealedValue})
	}
	return out
}

func webhookAuthDocOf(a endpoints.Auth) webhookAuthDoc {
	return webhookAuthDoc{Type: a.Type, Username: a.Username, PasswordCiphertext: a.SealedPassword, TokenCiphertext: a.SealedToken}
}

func webhookTLSDocOf(t endpoints.TLS) webhookTLSDoc {
	return webhookTLSDoc{CACertificate: t.CACertificate, ClientCertificate: t.ClientCertificate, ClientKeyCiphertext: t.SealedClientKey}
}

// WebhooksRepository implements endpoints.Repository.
type WebhooksRepository struct{ store *Store }

var _ endpoints.Repository = (*WebhooksRepository)(nil)

// NewWebhooksRepository returns the webhook endpoint repository.
func NewWebhooksRepository(s *Store) *WebhooksRepository { return &WebhooksRepository{store: s} }

// NewID returns a new endpoint ObjectID.
func (r *WebhooksRepository) NewID() string { return bson.NewObjectID().Hex() }

// webhookTokenHint fills the principal's token hint: webhook principals
// have no API credential.
const webhookTokenHint = "webhook"

// Create stores the endpoint with a dedicated webhook service principal.
func (r *WebhooksRepository) Create(ctx context.Context, e endpoints.Endpoint, principalName string, ev audit.Event) (endpoints.Endpoint, error) {
	ws, pid := mustOID(e.WorkspaceId), mustOID(e.ProjectId)
	doc := webhookEndpointDoc{Id: mustOID(e.Id), WorkspaceId: ws, ProjectId: pid, PrincipalId: bson.NewObjectID(), Name: e.Name, URL: e.URL,
		EventTypes: e.EventTypes, Active: e.Active, IncludeAgentNotes: e.IncludeAgentNotes, Headers: webhookHeaderDocs(e.Headers),
		Auth: webhookAuthDocOf(e.Auth), TLS: webhookTLSDocOf(e.TLS),
		CreatedBy: mustOID(e.CreatedBy), CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, PausedAt: e.PausedAt, Version: 1}
	principal := servicePrincipalDoc{Id: doc.PrincipalId, WorkspaceId: ws, ProjectId: pid, Kind: "webhook", Name: principalName,
		Scopes: endpoints.PrincipalScopes, Status: "active", TokenHint: webhookTokenHint, CreatedBy: doc.CreatedBy,
		CreatedAt: e.CreatedAt, Version: 1}
	ev.Target = "webhook/" + doc.Id.Hex()
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colServicePrincipals).InsertOne(ctx, principal); err != nil {
			return err
		}
		if _, err := r.store.col(colWebhookEndpoints).InsertOne(ctx, doc); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		return endpoints.Endpoint{}, err
	}
	return doc.toDomain(), nil
}

// List pages through a project's endpoints by ID.
func (r *WebhooksRepository) List(ctx context.Context, projectID string, after *pagination.Cursor, limit int) ([]endpoints.Endpoint, error) {
	filter, err := pageFilter(bson.D{{Key: "projectId", Value: mustOID(projectID)}}, "", after)
	if err != nil {
		return nil, err
	}
	docs, err := findAll[webhookEndpointDoc](ctx, r.store.col(colWebhookEndpoints), filter, pageOptions("", limit))
	if err != nil {
		return nil, err
	}
	out := make([]endpoints.Endpoint, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// Get finds an endpoint, optionally within a project.
func (r *WebhooksRepository) Get(ctx context.Context, projectID, id string) (endpoints.Endpoint, error) {
	eid, err := oid(id)
	if err != nil {
		return endpoints.Endpoint{}, endpoints.ErrNotFound
	}
	filter := bson.D{{Key: "_id", Value: eid}}
	if projectID != "" {
		filter = append(filter, bson.E{Key: "projectId", Value: mustOID(projectID)})
	}
	var doc webhookEndpointDoc
	err = r.store.col(colWebhookEndpoints).FindOne(ctx, filter).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return endpoints.Endpoint{}, endpoints.ErrNotFound
	}
	if err != nil {
		return endpoints.Endpoint{}, err
	}
	return doc.toDomain(), nil
}

// Update writes the mutable fields with a version check.
func (r *WebhooksRepository) Update(ctx context.Context, e endpoints.Endpoint, principalName string, expectedVersion int64, ev audit.Event) (endpoints.Endpoint, error) {
	var doc webhookEndpointDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		filter := bson.D{{Key: "_id", Value: mustOID(e.Id)}, {Key: "projectId", Value: mustOID(e.ProjectId)}}
		err := r.store.col(colWebhookEndpoints).FindOneAndUpdate(ctx, append(filter, bson.E{Key: "version", Value: expectedVersion}),
			bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: e.Name}, {Key: "url", Value: e.URL}, {Key: "eventTypes", Value: e.EventTypes}, {Key: "active", Value: e.Active},
				{Key: "includeAgentNotes", Value: e.IncludeAgentNotes}, {Key: "headers", Value: webhookHeaderDocs(e.Headers)},
				{Key: "auth", Value: webhookAuthDocOf(e.Auth)}, {Key: "tls", Value: webhookTLSDocOf(e.TLS)},
				{Key: "updatedAt", Value: e.UpdatedAt}, {Key: "pausedAt", Value: e.PausedAt}}},
				{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			if n, _ := r.store.col(colWebhookEndpoints).CountDocuments(ctx, filter); n > 0 {
				return endpoints.ErrVersionConflict
			}
			return endpoints.ErrNotFound
		}
		if err != nil {
			return err
		}
		if principalName != "" {
			if _, err := r.store.col(colServicePrincipals).UpdateOne(ctx, bson.D{{Key: "_id", Value: doc.PrincipalId}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: principalName}}}}); err != nil {
				return err
			}
		}
		if !e.IncludeAgentNotes && slices.Contains(ev.Changes, "includeAgentNotes") {
			// The agent-note grant was withdrawn: payloads stored for unsent
			// deliveries were built with staff fields (assignee). Drop them
			// so a retry rebuilds the payload under the public projection.
			// The body generation moves too, so a payload being built with
			// the grant right now is not stored either (OC-ASYNC-07).
			// The project ID lets the project_endpoint index serve the
			// update (endpoint_id alone scanned every delivery, OC-OPS-10).
			if err := r.store.invalidateDeliveryBodies(ctx, bson.D{{Key: "projectId", Value: doc.ProjectId}, {Key: "endpointId", Value: doc.Id},
				{Key: "status", Value: bson.D{{Key: "$ne", Value: notifications.StatusSent}}}}); err != nil {
				return err
			}
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return endpoints.Endpoint{}, err
	}
	return doc.toDomain(), nil
}

// Delete removes the endpoint and revokes its principal.
func (r *WebhooksRepository) Delete(ctx context.Context, projectID, id string, now time.Time, ev audit.Event) error {
	return r.store.transaction(ctx, func(ctx context.Context) error {
		var doc webhookEndpointDoc
		err := r.store.col(colWebhookEndpoints).FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return endpoints.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := r.store.col(colServicePrincipals).UpdateOne(ctx, bson.D{{Key: "_id", Value: doc.PrincipalId}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "revoked"}, {Key: "revokedAt", Value: now}}},
				{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}}); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
}

// PrincipalActive reports whether a webhook principal is active.
func (r *WebhooksRepository) PrincipalActive(ctx context.Context, principalID string) (bool, error) {
	n, err := r.store.col(colServicePrincipals).CountDocuments(ctx, bson.D{{Key: "_id", Value: mustOID(principalID)},
		{Key: "kind", Value: "webhook"}, {Key: "status", Value: "active"}})
	return n > 0, err
}

// Stats summarizes the deliveries of the given endpoints with one
// aggregation. The 24 hour window (attempts since a time, by lastAttemptAt)
// feeds the counts, median, last send and strip; "retrying" counts every
// delivery still waiting, however old. The two $or branches are served by
// the projectEndpointAttempt and projectEndpointWaiting indexes.
func (r *WebhooksRepository) Stats(ctx context.Context, projectID string, ids []string, since time.Time) (map[string]endpoints.Stats, error) {
	oids := make(bson.A, 0, len(ids))
	for _, id := range ids {
		oids = append(oids, mustOID(id))
	}
	waiting := bson.A{notifications.StatusRetrying, notifications.StatusPending}
	inWindow := bson.D{{Key: "$gte", Value: bson.A{"$lastAttemptAt", since}}}
	count := func(cond bson.D) bson.D {
		return bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{cond, 1, 0}}}}}
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "endpointId", Value: bson.D{{Key: "$in", Value: oids}}},
			{Key: "attempts", Value: bson.D{{Key: "$gte", Value: 1}}},
			{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{notifications.StatusSkipped, notifications.StatusSkippedUnconfigured}}}},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "lastAttemptAt", Value: bson.D{{Key: "$gte", Value: since}}}},
				bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: waiting}}}},
			}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$endpointId"},
			{Key: "sent", Value: count(inWindow)},
			{Key: "firstTry", Value: count(bson.D{{Key: "$and", Value: bson.A{inWindow,
				bson.D{{Key: "$eq", Value: bson.A{"$status", notifications.StatusSent}}}, bson.D{{Key: "$eq", Value: bson.A{"$attempts", 1}}}}}})},
			{Key: "retrying", Value: count(bson.D{{Key: "$in", Value: bson.A{"$status", waiting}}})},
			{Key: "lastSentAt", Value: bson.D{{Key: "$max", Value: bson.D{{Key: "$cond", Value: bson.A{inWindow, "$lastAttemptAt", nil}}}}}},
			{Key: "median", Value: bson.D{{Key: "$median", Value: bson.D{{Key: "input", Value: bson.D{{Key: "$cond", Value: bson.A{inWindow, "$responseMs", nil}}}}, {Key: "method", Value: "approximate"}}}}},
			{Key: "recent", Value: bson.D{{Key: "$topN", Value: bson.D{{Key: "n", Value: endpoints.StripSize},
				{Key: "sortBy", Value: bson.D{{Key: "lastAttemptAt", Value: -1}}},
				{Key: "output", Value: bson.D{{Key: "status", Value: "$status"}, {Key: "at", Value: "$lastAttemptAt"}}}}}}},
		}}},
	}
	cur, err := r.store.col(colDeliveries).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Id         bson.ObjectID `bson:"_id"`
		Sent       int           `bson:"sent"`
		FirstTry   int           `bson:"firstTry"`
		Retrying   int           `bson:"retrying"`
		LastSentAt *time.Time    `bson:"lastSentAt"`
		Median     *float64      `bson:"median"`
		Recent     []struct {
			Status string     `bson:"status"`
			At     *time.Time `bson:"at"`
		} `bson:"recent"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]endpoints.Stats, len(rows))
	for _, d := range rows {
		s := endpoints.Stats{Sent: d.Sent, FirstTry: d.FirstTry, Retrying: d.Retrying, LastSentAt: d.LastSentAt}
		if d.Median != nil {
			m := int(*d.Median + 0.5)
			s.MedianResponseMs = &m
		}
		for _, it := range d.Recent {
			// Old waiting deliveries are counted above but are not part of the strip.
			if it.At != nil && !it.At.Before(since) {
				s.Recent = append(s.Recent, endpoints.StripItem{Status: it.Status, At: *it.At})
			}
		}
		out[d.Id.Hex()] = s
	}
	return out, nil
}
