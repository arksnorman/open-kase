package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/arksnorman/open-kase/api/internal/idempotency"
)

type idempotencyDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	Scope       string        `bson:"scope"`
	Key         string        `bson:"key"`
	RequestHash string        `bson:"requestHash"`
	State       string        `bson:"state"`
	Status      int32         `bson:"status,omitempty"`
	Body        []byte        `bson:"body,omitempty"`
	Location    string        `bson:"location,omitempty"`
	LeaseUntil  time.Time     `bson:"leaseUntil"`
	CreatedAt   time.Time     `bson:"createdAt"`
	ExpiresAt   time.Time     `bson:"expiresAt"`
}

// IdempotencyRepository implements idempotency.Store.
type IdempotencyRepository struct{ store *Store }

// NewIdempotencyRepository returns an idempotency store.
func NewIdempotencyRepository(s *Store) *IdempotencyRepository {
	return &IdempotencyRepository{store: s}
}

// Begin reserves a key or returns the stored response for replay.
func (r *IdempotencyRepository) Begin(ctx context.Context, scope, key, requestHash string, now time.Time) (*idempotency.Record, error) {
	col := r.store.col(colIdempotency)
	doc := idempotencyDoc{Id: bson.NewObjectID(), Scope: scope, Key: key, RequestHash: requestHash, State: "pending",
		LeaseUntil: now.Add(idempotency.Lease), CreatedAt: now, ExpiresAt: now.Add(idempotency.Retention)}
	_, err := col.InsertOne(ctx, doc)
	if err == nil {
		return nil, nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return nil, err
	}
	var cur idempotencyDoc
	if err := col.FindOne(ctx, bson.D{{Key: "scope", Value: scope}, {Key: "key", Value: key}}).Decode(&cur); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, idempotency.ErrInProgress // removed concurrently; let the client retry
		}
		return nil, err
	}
	if cur.RequestHash != requestHash {
		return nil, idempotency.ErrMismatch
	}
	if cur.State == "completed" {
		return &idempotency.Record{Status: int(cur.Status), Body: cur.Body, Location: cur.Location}, nil
	}
	// Pending: take over only when the previous lease has expired.
	res, err := col.UpdateOne(ctx, bson.D{{Key: "_id", Value: cur.Id}, {Key: "state", Value: "pending"}, {Key: "leaseUntil", Value: bson.D{{Key: "$lte", Value: now}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "leaseUntil", Value: now.Add(idempotency.Lease)}}}})
	if err != nil {
		return nil, err
	}
	if res.ModifiedCount == 0 {
		return nil, idempotency.ErrInProgress
	}
	return nil, nil
}

// Complete stores the response for replay.
func (r *IdempotencyRepository) Complete(ctx context.Context, scope, key string, rec idempotency.Record, now time.Time) error {
	set := bson.D{{Key: "state", Value: "completed"}, {Key: "status", Value: int32(rec.Status)},
		{Key: "location", Value: rec.Location}, {Key: "expiresAt", Value: now.Add(idempotency.Retention)}}
	if len(rec.Body) > 0 {
		set = append(set, bson.E{Key: "body", Value: rec.Body})
	}
	_, err := r.store.col(colIdempotency).UpdateOne(ctx, bson.D{{Key: "scope", Value: scope}, {Key: "key", Value: key}},
		bson.D{{Key: "$set", Value: set}})
	return err
}

// Abandon deletes a pending reservation.
func (r *IdempotencyRepository) Abandon(ctx context.Context, scope, key string) error {
	// A reservation marked with a committed resource is kept: a retry must
	// find that resource instead of running the command again.
	_, err := r.store.col(colIdempotency).DeleteOne(ctx, bson.D{{Key: "scope", Value: scope}, {Key: "key", Value: key},
		{Key: "state", Value: "pending"}, {Key: "resource", Value: bson.D{{Key: "$exists", Value: false}}}})
	return err
}
