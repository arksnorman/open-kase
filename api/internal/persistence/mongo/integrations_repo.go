package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

type servicePrincipalDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	ProjectId   bson.ObjectID `bson:"projectId"`
	Kind        string        `bson:"kind"`
	Name        string        `bson:"name"`
	Scopes      []string      `bson:"scopes"`
	Status      string        `bson:"status"`
	TokenHint   string        `bson:"tokenHint"`
	CreatedBy   bson.ObjectID `bson:"createdBy"`
	CreatedAt   time.Time     `bson:"createdAt"`
	LastUsedAt  *time.Time    `bson:"lastUsedAt,omitempty"`
	RevokedAt   *time.Time    `bson:"revokedAt,omitempty"`
	Version     int64         `bson:"version"`
}

func (d servicePrincipalDoc) toDomain() integrations.Integration {
	return integrations.Integration{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Name: d.Name,
		Scopes: d.Scopes, Status: d.Status, TokenHint: d.TokenHint, CreatedBy: d.CreatedBy.Hex(), CreatedAt: d.CreatedAt,
		LastUsedAt: d.LastUsedAt, RevokedAt: d.RevokedAt, Version: d.Version}
}

type apiCredentialDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	ProjectId   bson.ObjectID `bson:"projectId"`
	PrincipalId bson.ObjectID `bson:"principalId"`
	TokenHash   string        `bson:"tokenHash"`
	CreatedAt   time.Time     `bson:"createdAt"`
	LastUsedAt  *time.Time    `bson:"lastUsedAt,omitempty"`
	RevokedAt   *time.Time    `bson:"revokedAt,omitempty"`
}

// IntegrationsRepository implements integrations.Repository.
type IntegrationsRepository struct{ store *Store }

// NewIntegrationsRepository returns an integrations repository.
func NewIntegrationsRepository(s *Store) *IntegrationsRepository {
	return &IntegrationsRepository{store: s}
}

// Create stores the service principal, its credential hash and the event.
func (r *IntegrationsRepository) Create(ctx context.Context, in integrations.Integration, tokenHash string, ev audit.Event) (integrations.Integration, error) {
	doc := servicePrincipalDoc{Id: bson.NewObjectID(), WorkspaceId: mustOID(in.WorkspaceId), ProjectId: mustOID(in.ProjectId),
		Kind: "integration", Name: in.Name, Scopes: in.Scopes, Status: in.Status, TokenHint: in.TokenHint,
		CreatedBy: mustOID(in.CreatedBy), CreatedAt: in.CreatedAt, Version: 1}
	cred := apiCredentialDoc{Id: bson.NewObjectID(), WorkspaceId: doc.WorkspaceId, ProjectId: doc.ProjectId,
		PrincipalId: doc.Id, TokenHash: tokenHash, CreatedAt: in.CreatedAt}
	ev.Target = "integration/" + doc.Id.Hex()
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colServicePrincipals).InsertOne(ctx, doc); err != nil {
			return err
		}
		if _, err := r.store.col(colAPICredentials).InsertOne(ctx, cred); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return integrations.Integration{}, err
	}
	return doc.toDomain(), nil
}

// List pages through a project's integrations, newest last.
func (r *IntegrationsRepository) List(ctx context.Context, workspaceID, projectID string, after *pagination.Cursor, limit int) ([]integrations.Integration, error) {
	filter, err := pageFilter(bson.D{{Key: "workspaceId", Value: mustOID(workspaceID)}, {Key: "projectId", Value: mustOID(projectID)},
		{Key: "kind", Value: "integration"}}, "", after)
	if err != nil {
		return nil, err
	}
	docs, err := findAll[servicePrincipalDoc](ctx, r.store.col(colServicePrincipals), filter, pageOptions("", limit))
	if err != nil {
		return nil, err
	}
	out := make([]integrations.Integration, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// Rename changes an active integration's name with a version check. An
// unchanged name is a no-op without an audit event.
func (r *IntegrationsRepository) Rename(ctx context.Context, workspaceID, projectID, id, name string, expectedVersion int64, ev audit.Event) (integrations.Integration, error) {
	pid, err := oid(id)
	if err != nil {
		return integrations.Integration{}, integrations.ErrNotFound
	}
	ws := mustOID(workspaceID)
	filter := bson.D{{Key: "_id", Value: pid}, {Key: "workspaceId", Value: ws},
		{Key: "projectId", Value: mustOID(projectID)}, {Key: "kind", Value: "integration"}}
	var doc servicePrincipalDoc
	err = r.store.transaction(ctx, func(ctx context.Context) error {
		err := r.store.col(colServicePrincipals).FindOne(ctx, filter).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return integrations.ErrNotFound
		}
		if err != nil {
			return err
		}
		if doc.Version != expectedVersion {
			return integrations.ErrVersionConflict
		}
		if doc.Status != integrations.StatusActive {
			return integrations.ErrRevoked
		}
		if doc.Name == name {
			return nil
		}
		err = r.store.col(colServicePrincipals).FindOneAndUpdate(ctx, append(filter, bson.E{Key: "version", Value: expectedVersion}), bson.D{
			{Key: "$set", Value: bson.D{{Key: "name", Value: name}}},
			{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}},
		}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return integrations.ErrVersionConflict
		}
		if err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		return integrations.Integration{}, err
	}
	return doc.toDomain(), nil
}

// Revoke revokes the principal and its credentials. Revoking an already
// revoked integration succeeds without a second audit event.
func (r *IntegrationsRepository) Revoke(ctx context.Context, workspaceID, projectID, id string, now time.Time, ev audit.Event) error {
	pid, err := oid(id)
	if err != nil {
		return integrations.ErrNotFound
	}
	ws := mustOID(workspaceID)
	return r.store.transaction(ctx, func(ctx context.Context) error {
		var doc servicePrincipalDoc
		err := r.store.col(colServicePrincipals).FindOne(ctx, bson.D{{Key: "_id", Value: pid}, {Key: "workspaceId", Value: ws},
			{Key: "projectId", Value: mustOID(projectID)}, {Key: "kind", Value: "integration"}}).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return integrations.ErrNotFound
		}
		if err != nil {
			return err
		}
		if doc.Status == integrations.StatusRevoked {
			return nil
		}
		if _, err := r.store.col(colServicePrincipals).UpdateOne(ctx, bson.D{{Key: "_id", Value: pid}}, bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: integrations.StatusRevoked}, {Key: "revokedAt", Value: now}}},
			{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}},
		}); err != nil {
			return err
		}
		if _, err := r.store.col(colAPICredentials).UpdateMany(ctx, bson.D{{Key: "principalId", Value: pid}, {Key: "revokedAt", Value: nil}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: now}}}}); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
}

// FindByTokenHash returns an unrevoked credential and its principal.
func (r *IntegrationsRepository) FindByTokenHash(ctx context.Context, hash string) (integrations.Credential, error) {
	var cred apiCredentialDoc
	err := r.store.col(colAPICredentials).FindOne(ctx, bson.D{{Key: "tokenHash", Value: hash}, {Key: "revokedAt", Value: nil}}).Decode(&cred)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return integrations.Credential{}, integrations.ErrNotFound
	}
	if err != nil {
		return integrations.Credential{}, err
	}
	var doc servicePrincipalDoc
	err = r.store.col(colServicePrincipals).FindOne(ctx, bson.D{{Key: "_id", Value: cred.PrincipalId}, {Key: "kind", Value: "integration"}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return integrations.Credential{}, integrations.ErrNotFound
	}
	if err != nil {
		return integrations.Credential{}, err
	}
	return integrations.Credential{Integration: doc.toDomain(), CredentialId: cred.Id.Hex()}, nil
}

// TouchLastUsed updates last-use timestamps at most once per interval.
func (r *IntegrationsRepository) TouchLastUsed(ctx context.Context, c integrations.Credential, now time.Time, interval time.Duration) error {
	stale := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "lastUsedAt", Value: bson.D{{Key: "$exists", Value: false}}}},
		bson.D{{Key: "lastUsedAt", Value: bson.D{{Key: "$lt", Value: now.Add(-interval)}}}},
	}}}
	set := bson.D{{Key: "$set", Value: bson.D{{Key: "lastUsedAt", Value: now}}}}
	if _, err := r.store.col(colAPICredentials).UpdateOne(ctx, append(bson.D{{Key: "_id", Value: mustOID(c.CredentialId)}}, stale...), set); err != nil {
		return err
	}
	_, err := r.store.col(colServicePrincipals).UpdateOne(ctx, append(bson.D{{Key: "_id", Value: mustOID(c.Integration.Id)}}, stale...), set)
	return err
}

// ProjectActive reports whether a project exists and is active.
func (r *IntegrationsRepository) ProjectActive(ctx context.Context, workspaceID, projectID string) (bool, error) {
	n, err := r.store.col(colProjects).CountDocuments(ctx, bson.D{{Key: "_id", Value: mustOID(projectID)},
		{Key: "workspaceId", Value: mustOID(workspaceID)}, {Key: "status", Value: "active"}})
	return n > 0, err
}
