package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/reports"
	"github.com/arksnorman/open-kase/api/internal/sla"
)

// ReportsRepository implements reports.Repository with bounded
// aggregations (each limited by a server-side time budget).
type ReportsRepository struct {
	store   *Store
	tickets *TicketsRepository
}

var _ reports.Repository = (*ReportsRepository)(nil)

// NewReportsRepository returns the reports repository.
func NewReportsRepository(s *Store) *ReportsRepository {
	return &ReportsRepository{store: s, tickets: NewTicketsRepository(s)}
}

// reportTimeout bounds each aggregation.
const reportTimeout = 10 * time.Second

func (r *ReportsRepository) aggregate(ctx context.Context, col string, pipeline bson.A, out any) error {
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	cur, err := r.store.col(col).Aggregate(ctx, pipeline)
	if err != nil {
		return err
	}
	return cur.All(ctx, out)
}

var openCategories = bson.A{"open", "waitingCustomer", "onHold"}

// Backlog groups live open work by status, priority and group.
func (r *ReportsRepository) Backlog(ctx context.Context, workspaceID, projectID string) (reports.Backlog, error) {
	match := bson.D{{Key: "$match", Value: bson.D{{Key: "projectId", Value: mustOID(projectID)},
		{Key: "statusCategory", Value: bson.D{{Key: "$in", Value: openCategories}}}}}}
	var facets []struct {
		Status []struct {
			Id       bson.ObjectID `bson:"_id"`
			Category string        `bson:"category"`
			Count    int64         `bson:"count"`
		} `bson:"status"`
		Priority []struct {
			Id    int32 `bson:"_id"`
			Count int64 `bson:"count"`
		} `bson:"priority"`
		Group []struct {
			Id    bson.ObjectID `bson:"_id"`
			Count int64         `bson:"count"`
		} `bson:"group"`
	}
	count := bson.D{{Key: "$sum", Value: 1}}
	err := r.aggregate(ctx, colTickets, bson.A{match, bson.D{{Key: "$facet", Value: bson.D{
		{Key: "status", Value: bson.A{bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$statusId"},
			{Key: "category", Value: bson.D{{Key: "$first", Value: "$statusCategory"}}}, {Key: "count", Value: count}}}}}},
		{Key: "priority", Value: bson.A{bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$priority"}, {Key: "count", Value: count}}}},
			bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}}}},
		{Key: "group", Value: bson.A{bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$groupId"}, {Key: "count", Value: count}}}},
			bson.D{{Key: "$sort", Value: bson.D{{Key: "count", Value: -1}}}}}},
	}}}}, &facets)
	if err != nil {
		return reports.Backlog{}, err
	}
	out := reports.Backlog{ByStatus: []reports.Count{}, ByPriority: []reports.Count{}, ByGroup: []reports.Count{}}
	if len(facets) == 0 {
		return out, nil
	}
	f := facets[0]
	ids := map[string]map[bson.ObjectID]bool{colStatuses: {}, colGroups: {}}
	for _, s := range f.Status {
		ids[colStatuses][s.Id] = true
	}
	for _, g := range f.Group {
		ids[colGroups][g.Id] = true
	}
	for k, v := range ids {
		if len(v) == 0 {
			delete(ids, k)
		}
	}
	names, err := r.tickets.names(ctx, ids)
	if err != nil {
		return reports.Backlog{}, err
	}
	for _, s := range f.Status {
		out.ByStatus = append(out.ByStatus, reports.Count{Id: s.Id.Hex(), Name: names[s.Id].name, Category: s.Category, Count: s.Count})
		out.Total += s.Count
	}
	for _, p := range f.Priority {
		out.ByPriority = append(out.ByPriority, reports.Count{Priority: int(p.Id), Count: p.Count})
	}
	for _, g := range f.Group {
		out.ByGroup = append(out.ByGroup, reports.Count{Id: g.Id.Hex(), Name: names[g.Id].name, Count: g.Count})
	}
	return out, nil
}

func (r *ReportsRepository) perDay(ctx context.Context, col string, match bson.D, dateField string) (map[string]int64, error) {
	var rows []struct {
		Day   string `bson:"_id"`
		Count int64  `bson:"count"`
	}
	err := r.aggregate(ctx, col, bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "$dateToString", Value: bson.D{{Key: "format", Value: "%Y-%m-%d"}, {Key: "date", Value: "$" + dateField}}}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
	}, &rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Day] = row.Count
	}
	return out, nil
}

func between(from, to time.Time) bson.D {
	return bson.D{{Key: "$gte", Value: from}, {Key: "$lt", Value: to}}
}

// CreatedPerDay counts live tickets created per UTC day.
func (r *ReportsRepository) CreatedPerDay(ctx context.Context, projectID string, from, to time.Time) (map[string]int64, error) {
	return r.perDay(ctx, colTickets, bson.D{{Key: "projectId", Value: mustOID(projectID)},
		{Key: "createdAt", Value: between(from, to)}}, "createdAt")
}

// EventsPerDay counts audit events of one action per UTC day.
func (r *ReportsRepository) EventsPerDay(ctx context.Context, projectID string, action audit.Action, from, to time.Time) (map[string]int64, error) {
	return r.perDay(ctx, colAuditEvents, bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "action", Value: action},
		{Key: "createdAt", Value: between(from, to)}}, "createdAt")
}

func (r *ReportsRepository) durations(ctx context.Context, filter bson.D, startField, endField string, limit int) ([]int64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	cur, err := r.store.col(colTickets).Find(ctx, filter, options.Find().
		SetProjection(bson.D{{Key: startField, Value: 1}, {Key: endField, Value: 1}}).SetLimit(int64(limit+1)))
	if err != nil {
		return nil, false, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, false, err
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		a, ok1 := row[startField].(bson.DateTime)
		b, ok2 := row[endField].(bson.DateTime)
		if !ok1 || !ok2 || b < a {
			continue
		}
		out = append(out, (int64(b)-int64(a))/1000)
	}
	return out, truncated, nil
}

// FirstResponseSeconds samples tickets created in the range that have a
// first public staff reply.
func (r *ReportsRepository) FirstResponseSeconds(ctx context.Context, projectID string, from, to time.Time, limit int) ([]int64, bool, error) {
	return r.durations(ctx, bson.D{{Key: "projectId", Value: mustOID(projectID)},
		{Key: "createdAt", Value: between(from, to)}, {Key: "firstResponseAt", Value: bson.D{{Key: "$type", Value: "date"}}}},
		"createdAt", "firstResponseAt", limit)
}

// ResolutionSeconds samples tickets resolved in the range (creation to the
// latest resolution).
func (r *ReportsRepository) ResolutionSeconds(ctx context.Context, projectID string, from, to time.Time, limit int) ([]int64, bool, error) {
	return r.durations(ctx, bson.D{{Key: "projectId", Value: mustOID(projectID)},
		{Key: "resolvedAt", Value: between(from, to)}}, "createdAt", "resolvedAt", limit)
}

// SLABreaches counts first-response breaches recorded in the range.
func (r *ReportsRepository) SLABreaches(ctx context.Context, projectID string, from, to time.Time) (int64, error) {
	var rows []struct {
		Count int64 `bson:"count"`
	}
	err := r.aggregate(ctx, colSLAInstances, bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "breaches.at", Value: between(from, to)}}}},
		bson.D{{Key: "$unwind", Value: "$breaches"}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "breaches.at", Value: between(from, to)}, {Key: "breaches.kind", Value: string(sla.FirstResponse)}}}},
		bson.D{{Key: "$count", Value: "count"}},
	}, &rows)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].Count, nil
}
