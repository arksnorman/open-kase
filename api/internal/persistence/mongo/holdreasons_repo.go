package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/statuses"
)

type holdReasonDoc struct {
	Id             bson.ObjectID  `bson:"_id"`
	WorkspaceId    bson.ObjectID  `bson:"workspaceId"`
	ProjectId      bson.ObjectID  `bson:"projectId"`
	Name           string         `bson:"name"`
	NameNormalized string         `bson:"nameNormalized"`
	MaxDays        int32          `bson:"maxDays"`
	CreatedBy      *bson.ObjectID `bson:"createdBy"`
	CreatedAt      time.Time      `bson:"createdAt"`
	LastHoldAt     *time.Time     `bson:"lastHoldAt,omitempty"`
	Version        int64          `bson:"version"`
}

func newHoldReasonDoc(r holdreasons.Reason) holdReasonDoc {
	return holdReasonDoc{Id: mustOID(r.Id), WorkspaceId: mustOID(r.WorkspaceId), ProjectId: mustOID(r.ProjectId), Name: r.Name,
		NameNormalized: holdreasons.NormalizeName(r.Name), MaxDays: int32(r.MaxDays), CreatedBy: optOID(r.CreatedBy),
		CreatedAt: r.CreatedAt, Version: r.Version}
}

func (d holdReasonDoc) toDomain() holdreasons.Reason {
	return holdreasons.Reason{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Name: d.Name,
		MaxDays: int(d.MaxDays), CreatedBy: hexOrEmpty(d.CreatedBy), CreatedAt: d.CreatedAt, Version: d.Version}
}

// HoldReasonsRepository implements holdreasons.Repository.
type HoldReasonsRepository struct{ store *Store }

var _ holdreasons.Repository = (*HoldReasonsRepository)(nil)

// NewHoldReasonsRepository returns the hold reasons repository.
func NewHoldReasonsRepository(s *Store) *HoldReasonsRepository {
	return &HoldReasonsRepository{store: s}
}

// List returns the project's hold reasons by name; withUsage counts the
// tickets currently on hold with each.
func (r *HoldReasonsRepository) List(ctx context.Context, projectID string, withUsage bool) ([]holdreasons.Reason, error) {
	pid := mustOID(projectID)
	docs, err := findAll[holdReasonDoc](ctx, r.store.col(colHoldReasons), bson.D{{Key: "projectId", Value: pid}},
		options.Find().SetSort(bson.D{{Key: "nameNormalized", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(holdreasons.MaxReasons))
	if err != nil {
		return nil, err
	}
	counts := map[bson.ObjectID]int64{}
	if withUsage && len(docs) > 0 {
		cur, err := r.store.col(colTickets).Aggregate(ctx, bson.A{
			bson.D{{Key: "$match", Value: bson.D{{Key: "projectId", Value: pid}, {Key: "statusCategory", Value: string(statuses.CategoryOnHold)}}}},
			bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$holdReasonId"}, {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
		})
		if err != nil {
			return nil, err
		}
		var rows []struct {
			Id    *bson.ObjectID `bson:"_id"`
			Count int64          `bson:"count"`
		}
		if err := cur.All(ctx, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Id != nil {
				counts[*row.Id] = row.Count
			}
		}
	}
	out := make([]holdreasons.Reason, 0, len(docs))
	for _, d := range docs {
		rs := d.toDomain()
		rs.InUse = counts[d.Id]
		out = append(out, rs)
	}
	return out, nil
}

// Get returns one hold reason.
func (r *HoldReasonsRepository) Get(ctx context.Context, projectID, id string) (holdreasons.Reason, error) {
	var d holdReasonDoc
	err := r.store.col(colHoldReasons).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return holdreasons.Reason{}, holdreasons.ErrNotFound
	}
	if err != nil {
		return holdreasons.Reason{}, err
	}
	return d.toDomain(), nil
}

// Create inserts a hold reason within the project limit.
func (r *HoldReasonsRepository) Create(ctx context.Context, rs holdreasons.Reason, ev audit.Event) (holdreasons.Reason, error) {
	rs.Id = bson.NewObjectID().Hex()
	doc := newHoldReasonDoc(rs)
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if err := r.store.bumpCounter(ctx, "holdReasons:"+doc.ProjectId.Hex()); err != nil {
			return err
		}
		n, err := r.store.col(colHoldReasons).CountDocuments(ctx, bson.D{{Key: "projectId", Value: doc.ProjectId}})
		if err != nil {
			return err
		}
		if n >= holdreasons.MaxReasons {
			return holdreasons.ErrLimit
		}
		if _, err := r.store.col(colHoldReasons).InsertOne(ctx, doc); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return holdreasons.ErrExists
			}
			return err
		}
		ev.Target = "holdReason/" + doc.Id.Hex()
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return holdreasons.Reason{}, err
	}
	return doc.toDomain(), nil
}

// Update changes name and maxDays at the expected version.
func (r *HoldReasonsRepository) Update(ctx context.Context, rs holdreasons.Reason, expectedVersion int64, ev audit.Event) (holdreasons.Reason, error) {
	var out holdReasonDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		err := r.store.col(colHoldReasons).FindOneAndUpdate(ctx,
			bson.D{{Key: "_id", Value: mustOID(rs.Id)}, {Key: "projectId", Value: mustOID(rs.ProjectId)}, {Key: "version", Value: expectedVersion}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: rs.Name}, {Key: "nameNormalized", Value: holdreasons.NormalizeName(rs.Name)},
				{Key: "maxDays", Value: int32(rs.MaxDays)}}}, {Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return holdreasons.ErrConflict
		}
		if mongo.IsDuplicateKeyError(err) {
			return holdreasons.ErrExists
		}
		if err != nil {
			return err
		}
		return r.store.insertAudit(ctx, out.WorkspaceId, ev)
	})
	if err != nil {
		return holdreasons.Reason{}, err
	}
	return out.toDomain(), nil
}

// Delete removes a hold reason unless a ticket is on hold with it. Putting
// a ticket on hold writes the reason document in its own transaction, so a
// concurrent hold and delete conflict and the loser re-checks.
func (r *HoldReasonsRepository) Delete(ctx context.Context, rs holdreasons.Reason, expectedVersion int64, ev audit.Event) error {
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colHoldReasons).DeleteOne(ctx,
			bson.D{{Key: "_id", Value: mustOID(rs.Id)}, {Key: "projectId", Value: mustOID(rs.ProjectId)}, {Key: "version", Value: expectedVersion}})
		if err != nil {
			return err
		}
		if res.DeletedCount == 0 {
			return holdreasons.ErrConflict
		}
		n, err := r.store.col(colTickets).CountDocuments(ctx, bson.D{{Key: "projectId", Value: mustOID(rs.ProjectId)},
			{Key: "statusCategory", Value: string(statuses.CategoryOnHold)}, {Key: "holdReasonId", Value: mustOID(rs.Id)}}, options.Count().SetLimit(1))
		if err != nil {
			return err
		}
		if n > 0 {
			return holdreasons.ErrInUse
		}
		return r.store.insertAudit(ctx, mustOID(rs.WorkspaceId), ev)
	})
}
