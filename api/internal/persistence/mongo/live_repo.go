package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/live"
)

// LiveSource reads committed ticket changes from the outbox for live
// update streams.
type LiveSource struct{ store *Store }

var _ live.Source = (*LiveSource)(nil)

// NewLiveSource returns the outbox change source.
func NewLiveSource(s *Store) *LiveSource { return &LiveSource{store: s} }

// ChangesSince returns ticket outbox events of the projects whose ObjectID
// (wall-clock) time is at or after since, oldest first. Every state counts
// (pending, dispatched, suppressed, failed): the stream reports committed
// changes, not deliveries.
func (s *LiveSource) ChangesSince(ctx context.Context, projectIDs []string, since time.Time, limit int) ([]live.Change, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	docs, err := findAll[outboxDoc](ctx, s.store.col(colOutbox), bson.D{
		{Key: "_id", Value: bson.D{{Key: "$gte", Value: bson.NewObjectIDFromTimestamp(since)}}},
		{Key: "projectId", Value: bson.D{{Key: "$in", Value: oidList(projectIDs)}}},
		{Key: "ticketId", Value: bson.D{{Key: "$ne", Value: nil}}},
	}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)).
		SetProjection(bson.D{{Key: "projectId", Value: 1}, {Key: "ticketId", Value: 1}, {Key: "type", Value: 1},
			{Key: "visibility", Value: 1}, {Key: "ticketVersion", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]live.Change, 0, len(docs))
	for _, d := range docs {
		out = append(out, live.Change{Id: d.Id.Hex(), ProjectId: hexOrEmpty(d.ProjectId), TicketId: hexOrEmpty(d.TicketId), Type: d.Type,
			Visibility: d.Visibility, Version: d.TicketVersion, At: d.Id.Timestamp()})
	}
	return out, nil
}
