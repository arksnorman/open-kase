package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/requesttypes"
)

// requestTypeDoc is a request type. Types created by v0.3.0 (and by init)
// have no version and no lastUsedAt, and projectId is null: they are
// installation-wide. A missing version reads as 1, and a write sets the
// version explicitly.
type requestTypeDoc struct {
	Id          bson.ObjectID  `bson:"_id"`
	WorkspaceId bson.ObjectID  `bson:"workspaceId"`
	ProjectId   *bson.ObjectID `bson:"projectId"`
	Code        string         `bson:"code"`
	Name        string         `bson:"name"`
	Active      bool           `bson:"active"`
	Order       int64          `bson:"order"`
	CreatedAt   time.Time      `bson:"createdAt"`
	Version     int64          `bson:"version,omitempty"`
	LastUsedAt  *time.Time     `bson:"lastUsedAt,omitempty"`
}

func (d requestTypeDoc) toDomain() requesttypes.Type {
	v := d.Version
	if v < 1 {
		v = 1
	}
	return requesttypes.Type{Id: d.Id.Hex(), Code: d.Code, Name: d.Name, Order: int(d.Order), Active: d.Active, CreatedAt: d.CreatedAt, Version: v}
}

// RequestTypesRepository implements requesttypes.Repository.
type RequestTypesRepository struct{ store *Store }

var _ requesttypes.Repository = (*RequestTypesRepository)(nil)

// NewRequestTypesRepository returns the request types repository.
func NewRequestTypesRepository(s *Store) *RequestTypesRepository {
	return &RequestTypesRepository{store: s}
}

// List returns the installation-wide and the project's request types by
// order; withUsage counts the tickets carrying each.
func (r *RequestTypesRepository) List(ctx context.Context, workspaceID, projectID string, withUsage bool) ([]requesttypes.Type, error) {
	pid := mustOID(projectID)
	docs, err := findAll[requestTypeDoc](ctx, r.store.col(colRequestTypes),
		bson.D{{Key: "workspaceId", Value: mustOID(workspaceID)}, {Key: "projectId", Value: bson.D{{Key: "$in", Value: bson.A{nil, pid}}}}},
		options.Find().SetSort(bson.D{{Key: "order", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(500))
	if err != nil {
		return nil, err
	}
	counts := map[bson.ObjectID]int64{}
	if withUsage && len(docs) > 0 {
		cur, err := r.store.col(colTickets).Aggregate(ctx, bson.A{
			bson.D{{Key: "$match", Value: bson.D{{Key: "projectId", Value: pid}}}},
			bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$requestTypeId"}, {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
		})
		if err != nil {
			return nil, err
		}
		var rows []struct {
			Id    bson.ObjectID `bson:"_id"`
			Count int64         `bson:"count"`
		}
		if err := cur.All(ctx, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			counts[row.Id] = row.Count
		}
	}
	out := make([]requesttypes.Type, 0, len(docs))
	for _, d := range docs {
		t := d.toDomain()
		t.InUse = counts[d.Id]
		out = append(out, t)
	}
	return out, nil
}

// Change applies one management command. The counter write serializes the
// commands of a project, so the invariants plan checks against the types it
// reads hold when the plan is applied. Ticket writes touch the type they
// carry (touchRequestType), so a concurrent delete and ticket write
// conflict and the loser re-checks.
func (r *RequestTypesRepository) Change(ctx context.Context, workspaceID, projectID string, plan func([]requesttypes.Type) (requesttypes.Plan, error)) ([]requesttypes.Type, error) {
	ws, pid := mustOID(workspaceID), mustOID(projectID)
	var out []requesttypes.Type
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		out = nil
		if err := r.store.bumpCounter(ctx, "requestTypes:"+projectID); err != nil {
			return err
		}
		cur, err := r.List(ctx, workspaceID, projectID, false)
		if err != nil {
			return err
		}
		p, err := plan(cur)
		if err != nil {
			return err
		}
		if p.Insert == nil && len(p.Update) == 0 && p.Delete == "" {
			out, err = r.List(ctx, workspaceID, projectID, true)
			return err
		}
		col := r.store.col(colRequestTypes)
		if t := p.Insert; t != nil {
			t.Id = bson.NewObjectID().Hex()
			// projectId stays null like the seeded types, so the unique
			// (workspaceId, projectId, code) index spans old and new documents.
			doc := requestTypeDoc{Id: mustOID(t.Id), WorkspaceId: ws, Code: t.Code, Name: t.Name, Active: t.Active,
				Order: int64(t.Order), CreatedAt: t.CreatedAt, Version: t.Version}
			if _, err := col.InsertOne(ctx, doc); err != nil {
				if mongo.IsDuplicateKeyError(err) {
					return requesttypes.ErrCodeExists
				}
				return err
			}
			if p.Audit != nil && p.Audit.Target == "" {
				p.Audit.Target = "requestType/" + t.Id
			}
		}
		for _, u := range p.Update {
			set := bson.D{{Key: "version", Value: u.Version}}
			if u.Name != nil {
				set = append(set, bson.E{Key: "name", Value: *u.Name})
			}
			if u.Active != nil {
				set = append(set, bson.E{Key: "active", Value: *u.Active})
			}
			if u.Order != nil {
				set = append(set, bson.E{Key: "order", Value: int64(*u.Order)})
			}
			res, err := col.UpdateOne(ctx, bson.D{{Key: "_id", Value: mustOID(u.Id)}, {Key: "workspaceId", Value: ws}}, bson.D{{Key: "$set", Value: set}})
			if err != nil {
				return err
			}
			if res.MatchedCount == 0 {
				return requesttypes.ErrNotFound
			}
		}
		if p.Delete != "" {
			id := mustOID(p.Delete)
			res, err := col.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "workspaceId", Value: ws}})
			if err != nil {
				return err
			}
			if res.DeletedCount == 0 {
				return requesttypes.ErrNotFound
			}
			n, err := r.store.col(colTickets).CountDocuments(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "requestTypeId", Value: id}},
				options.Count().SetLimit(1))
			if err != nil {
				return err
			}
			if n > 0 {
				return requesttypes.ErrInUse
			}
		}
		if p.Audit != nil {
			if err := r.store.insertAudit(ctx, ws, *p.Audit); err != nil {
				return err
			}
		}
		out, err = r.List(ctx, workspaceID, projectID, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// touchRequestType proves, inside the caller's transaction, that the request
// type still exists and writes it, so a ticket write conflicts with a
// concurrent delete or management write. It returns whether the type exists
// and whether it is active.
func (s *Store) touchRequestType(ctx context.Context, id bson.ObjectID, now time.Time) (found, active bool, err error) {
	var d requestTypeDoc
	err = s.col(colRequestTypes).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "lastUsedAt", Value: now}}}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, false, nil
	}
	return err == nil, d.Active, err
}
