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
	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// ---- Tags ----

type tagDoc struct {
	Id          bson.ObjectID  `bson:"_id"`
	WorkspaceId bson.ObjectID  `bson:"workspaceId"`
	ProjectId   bson.ObjectID  `bson:"projectId"`
	Name        string         `bson:"name"`
	CreatedAt   time.Time      `bson:"createdAt"`
	CreatedBy   *bson.ObjectID `bson:"createdBy"`
}

// TagCatalog returns the project's catalog tags by name with the number of
// tickets acc may read that carry each. The access filter is part of
// the aggregation, so no count includes a ticket the reader cannot see.
func (r *TicketsRepository) TagCatalog(ctx context.Context, projectID string, acc tickets.Access) ([]tickets.TagCount, error) {
	pid := mustOID(projectID)
	docs, err := findAll[tagDoc](ctx, r.store.col(colTags), bson.D{{Key: "projectId", Value: pid}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetLimit(tickets.MaxCatalogTags))
	if err != nil || len(docs) == 0 {
		return []tickets.TagCount{}, err
	}
	names := make(bson.A, 0, len(docs))
	for _, d := range docs {
		names = append(names, d.Name)
	}
	match := bson.D{{Key: "projectId", Value: pid}, {Key: "tags", Value: bson.D{{Key: "$in", Value: names}}}}
	if c := accessClause(acc); c != nil {
		match = append(match, c...)
	}
	cur, err := r.store.col(colTickets).Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$unwind", Value: "$tags"}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$tags"}, {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
	})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Tag   string `bson:"_id"`
		Count int64  `bson:"count"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.Tag] = row.Count
	}
	out := make([]tickets.TagCount, 0, len(docs))
	for _, d := range docs {
		out = append(out, tickets.TagCount{Tag: d.Name, Count: counts[d.Name], CreatedAt: d.CreatedAt})
	}
	return out, nil
}

// UnknownTags returns those of names that are not in the project's catalog.
func (r *TicketsRepository) UnknownTags(ctx context.Context, projectID string, names []string) ([]string, error) {
	docs, err := findAll[tagDoc](ctx, r.store.col(colTags), bson.D{{Key: "projectId", Value: mustOID(projectID)},
		{Key: "name", Value: bson.D{{Key: "$in", Value: names}}}}, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if !slices.ContainsFunc(docs, func(d tagDoc) bool { return d.Name == n }) {
			out = append(out, n)
		}
	}
	return out, nil
}

// CreateTag inserts a catalog tag within the project limit.
func (r *TicketsRepository) CreateTag(ctx context.Context, workspaceID, projectID, name, createdBy string, now time.Time, ev audit.Event) error {
	doc := tagDoc{Id: bson.NewObjectID(), WorkspaceId: mustOID(workspaceID), ProjectId: mustOID(projectID), Name: name, CreatedAt: now,
		CreatedBy: optOID(createdBy)}
	return r.store.transaction(ctx, func(ctx context.Context) error {
		if err := r.store.bumpCounter(ctx, "tags:"+projectID); err != nil {
			return err
		}
		n, err := r.store.col(colTags).CountDocuments(ctx, bson.D{{Key: "projectId", Value: doc.ProjectId}})
		if err != nil {
			return err
		}
		if n >= tickets.MaxCatalogTags {
			return tickets.ErrTagLimit
		}
		if _, err := r.store.col(colTags).InsertOne(ctx, doc); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return tickets.ErrTagExists
			}
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
}

// DeleteTag removes a catalog tag and pulls it from the project's tickets
// (archived ones included), bumping their versions so a stale
// edit cannot go unnoticed.
func (r *TicketsRepository) DeleteTag(ctx context.Context, workspaceID, projectID, name string, ev audit.Event) (int64, error) {
	pid := mustOID(projectID)
	var n int64
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colTags).DeleteOne(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "name", Value: name}})
		if err != nil {
			return err
		}
		if res.DeletedCount == 0 {
			return tickets.ErrTagNotFound
		}
		up, err := r.store.col(colTickets).UpdateMany(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "tags", Value: name}},
			bson.D{{Key: "$pull", Value: bson.D{{Key: "tags", Value: name}}}, {Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}})
		if err != nil {
			return err
		}
		n = up.ModifiedCount
		return r.store.insertAudit(ctx, mustOID(workspaceID), ev)
	})
	return n, err
}

// ---- Macros ----

type macroDoc struct {
	Id          bson.ObjectID  `bson:"_id"`
	WorkspaceId bson.ObjectID  `bson:"workspaceId"`
	ProjectId   bson.ObjectID  `bson:"projectId"`
	Name        string         `bson:"name"`
	Body        string         `bson:"body"`
	Visibility  string         `bson:"visibility"`
	Active      bool           `bson:"active"`
	CreatedBy   *bson.ObjectID `bson:"createdBy"`
	CreatedAt   time.Time      `bson:"createdAt"`
	UpdatedAt   time.Time      `bson:"updatedAt"`
	Version     int64          `bson:"version"`
}

func newMacroDoc(m macros.Macro) macroDoc {
	return macroDoc{Id: mustOID(m.Id), WorkspaceId: mustOID(m.WorkspaceId), ProjectId: mustOID(m.ProjectId), Name: m.Name, Body: m.Body,
		Visibility: m.Visibility, Active: m.Active, CreatedBy: optOID(m.CreatedBy), CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		Version: m.Version}
}

func (d macroDoc) toDomain() macros.Macro {
	return macros.Macro{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Name: d.Name, Body: d.Body,
		Visibility: d.Visibility, Active: d.Active, CreatedBy: hexOrEmpty(d.CreatedBy), CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		Version: d.Version}
}

// MacrosRepository implements macros.Repository.
type MacrosRepository struct{ store *Store }

var _ macros.Repository = (*MacrosRepository)(nil)

// NewMacrosRepository returns the macros repository.
func NewMacrosRepository(s *Store) *MacrosRepository { return &MacrosRepository{store: s} }

// ListMacros returns the project's macros by name.
func (r *MacrosRepository) ListMacros(ctx context.Context, projectID string) ([]macros.Macro, error) {
	docs, err := findAll[macroDoc](ctx, r.store.col(colMacros), bson.D{{Key: "projectId", Value: mustOID(projectID)}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(macros.MaxMacros))
	if err != nil {
		return nil, err
	}
	out := make([]macros.Macro, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// GetMacro returns one macro.
func (r *MacrosRepository) GetMacro(ctx context.Context, projectID, id string) (macros.Macro, error) {
	var d macroDoc
	err := r.store.col(colMacros).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return macros.Macro{}, macros.ErrNotFound
	}
	if err != nil {
		return macros.Macro{}, err
	}
	return d.toDomain(), nil
}

// bumpCounter increments a per-project counter document inside a
// transaction, serializing concurrent creates so a limit check cannot be
// raced past.
func (s *Store) bumpCounter(ctx context.Context, id string) error {
	_, err := s.col(colCounters).UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(1)}}}}, options.UpdateOne().SetUpsert(true))
	return err
}

// CreateMacro inserts a macro within the project limit.
func (r *MacrosRepository) CreateMacro(ctx context.Context, m macros.Macro, ev audit.Event) (macros.Macro, error) {
	m.Id = bson.NewObjectID().Hex()
	doc := newMacroDoc(m)
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if err := r.store.bumpCounter(ctx, "macros:"+doc.ProjectId.Hex()); err != nil {
			return err
		}
		n, err := r.store.col(colMacros).CountDocuments(ctx, bson.D{{Key: "projectId", Value: doc.ProjectId}})
		if err != nil {
			return err
		}
		if n >= macros.MaxMacros {
			return macros.ErrLimit
		}
		if _, err := r.store.col(colMacros).InsertOne(ctx, doc); err != nil {
			return err
		}
		ev.Target = "macro/" + doc.Id.Hex()
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return macros.Macro{}, err
	}
	return doc.toDomain(), nil
}

// UpdateMacro replaces a macro at the expected version.
func (r *MacrosRepository) UpdateMacro(ctx context.Context, m macros.Macro, expectedVersion int64, ev audit.Event) (macros.Macro, error) {
	m.Version = expectedVersion + 1
	doc := newMacroDoc(m)
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colMacros).ReplaceOne(ctx, bson.D{{Key: "_id", Value: doc.Id}, {Key: "version", Value: expectedVersion}}, doc)
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return macros.ErrConflict
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return macros.Macro{}, err
	}
	return doc.toDomain(), nil
}

// DeleteMacro removes a macro.
func (r *MacrosRepository) DeleteMacro(ctx context.Context, m macros.Macro, ev audit.Event) error {
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colMacros).DeleteOne(ctx, bson.D{{Key: "_id", Value: mustOID(m.Id)}, {Key: "projectId", Value: mustOID(m.ProjectId)}})
		if err != nil {
			return err
		}
		if res.DeletedCount == 0 {
			return macros.ErrNotFound
		}
		return r.store.insertAudit(ctx, mustOID(m.WorkspaceId), ev)
	})
}
