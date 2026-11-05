package mongo

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/auth"
)

// ---- Signing keys ----

type signingKeyDoc struct {
	Id                string        `bson:"_id"`
	WorkspaceId       bson.ObjectID `bson:"workspaceId"`
	Algorithm         string        `bson:"algorithm"`
	PublicKey         string        `bson:"publicKey"`
	PrivateCiphertext string        `bson:"privateCiphertext"`
	Active            bool          `bson:"active"`
	CreatedAt         time.Time     `bson:"createdAt"`
	RetiredAt         *time.Time    `bson:"retiredAt,omitempty"`
	VerifyUntil       *time.Time    `bson:"verifyUntil,omitempty"`
}

func (d signingKeyDoc) toDomain() auth.StoredKey {
	return auth.StoredKey{Id: d.Id, WorkspaceId: d.WorkspaceId.Hex(), Algorithm: d.Algorithm, PublicKey: d.PublicKey,
		PrivateCiphertext: d.PrivateCiphertext, Active: d.Active, CreatedAt: d.CreatedAt, RetiredAt: d.RetiredAt, VerifyUntil: d.VerifyUntil}
}

// SigningKeysRepository implements auth.KeyRepository.
type SigningKeysRepository struct{ store *Store }

// NewSigningKeysRepository returns a signing-key repository.
func NewSigningKeysRepository(s *Store) *SigningKeysRepository {
	return &SigningKeysRepository{store: s}
}

func (r *SigningKeysRepository) find(ctx context.Context, filter bson.D) (auth.StoredKey, error) {
	var doc signingKeyDoc
	err := r.store.col(colSigningKeys).FindOne(ctx, filter).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.StoredKey{}, auth.ErrKeyNotFound
	}
	if err != nil {
		return auth.StoredKey{}, err
	}
	return doc.toDomain(), nil
}

// ActiveKey returns the workspace's active signing key.
func (r *SigningKeysRepository) ActiveKey(ctx context.Context, workspaceID string) (auth.StoredKey, error) {
	ws, err := oid(workspaceID)
	if err != nil {
		return auth.StoredKey{}, auth.ErrKeyNotFound
	}
	return r.find(ctx, bson.D{{Key: "workspaceId", Value: ws}, {Key: "active", Value: true}})
}

// KeyByID returns any (active or retired) key for verification.
func (r *SigningKeysRepository) KeyByID(ctx context.Context, workspaceID, id string) (auth.StoredKey, error) {
	ws, err := oid(workspaceID)
	if err != nil {
		return auth.StoredKey{}, auth.ErrKeyNotFound
	}
	return r.find(ctx, bson.D{{Key: "_id", Value: id}, {Key: "workspaceId", Value: ws}})
}

// InsertKey stores a new active key; a concurrent creator wins through the
// partial unique index and this call returns auth.ErrKeyExists.
func (r *SigningKeysRepository) InsertKey(ctx context.Context, k auth.StoredKey, ev audit.Event) error {
	ws, err := oid(k.WorkspaceId)
	if err != nil {
		return err
	}
	err = r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colSigningKeys).InsertOne(ctx, signingKeyDoc{Id: k.Id, WorkspaceId: ws, Algorithm: k.Algorithm,
			PublicKey: k.PublicKey, PrivateCiphertext: k.PrivateCiphertext, Active: true, CreatedAt: k.CreatedAt}); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if mongo.IsDuplicateKeyError(err) {
		return auth.ErrKeyExists
	}
	return err
}

// ---- Login throttle ----

// ThrottleRule bounds failures per key within a fixed window. Once the limit
// is reached the key is blocked until the window ends.
type ThrottleRule struct {
	Limit  int
	Window time.Duration
}

// Throttle is a MongoDB-backed failure counter shared by every API process.
// It does not depend on Redis, so protection survives Redis loss.
type Throttle struct {
	store *Store
	now   func() time.Time
	rule  func(key string) ThrottleRule
}

// DefaultThrottleRule allows, per 15 minutes, 5 sign-in failures per
// identifier and per account from one client, an IPv4 address or IPv6 /64
// (login:id-ip:, login:user-ip:), 50 per identifier and per account from
// all clients together (login:id:, login:user:, the backstop against
// distributed guessing) and 50 per client IP (login:ip:). Other keys allow
// 5 failures per 15 minutes (MFA codes, re-authentication and password
// changes, keyed per user).
//
// Password reset requests count every request (not only failures): 3 per
// address and 20 per client IP per hour.
func DefaultThrottleRule(key string) ThrottleRule {
	switch {
	case strings.HasPrefix(key, "login:ip:"), strings.HasPrefix(key, "login:id:"), strings.HasPrefix(key, "login:user:"):
		return ThrottleRule{Limit: 50, Window: 15 * time.Minute}
	case strings.HasPrefix(key, "reset:email:"):
		return ThrottleRule{Limit: 3, Window: time.Hour}
	case strings.HasPrefix(key, "reset:ip:"):
		return ThrottleRule{Limit: 20, Window: time.Hour}
	}
	return ThrottleRule{Limit: 5, Window: 15 * time.Minute}
}

// NewThrottle returns a throttle using rule (DefaultThrottleRule when nil).
func NewThrottle(s *Store, now func() time.Time, rule func(string) ThrottleRule) *Throttle {
	if rule == nil {
		rule = DefaultThrottleRule
	}
	return &Throttle{store: s, now: now, rule: rule}
}

type throttleDoc struct {
	Id          string    `bson:"_id"`
	Failures    int64     `bson:"failures"`
	WindowStart time.Time `bson:"windowStart"`
}

// Check returns how long the caller must wait, or 0 when allowed.
func (t *Throttle) Check(ctx context.Context, keys []string) (time.Duration, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	docs, err := findAll[throttleDoc](ctx, t.store.col(colLoginThrottle), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: keys}}}}, options.Find())
	if err != nil {
		return 0, err
	}
	now := t.now()
	var wait time.Duration
	for _, d := range docs {
		rule := t.rule(d.Id)
		end := d.WindowStart.Add(rule.Window)
		if d.Failures >= int64(rule.Limit) && now.Before(end) {
			wait = max(wait, end.Sub(now))
		}
	}
	return wait, nil
}

// countPipeline adds one attempt to a key's window (starting a new window
// when the old one expired).
func (t *Throttle) countPipeline(rule ThrottleRule, now time.Time) mongo.Pipeline {
	expired := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "$not", Value: bson.A{"$windowStart"}}},
		bson.D{{Key: "$lte", Value: bson.A{"$windowStart", now.Add(-rule.Window)}}},
	}}}
	return mongo.Pipeline{{{Key: "$set", Value: bson.D{
		{Key: "failures", Value: bson.D{{Key: "$cond", Value: bson.A{expired, int64(1), bson.D{{Key: "$add", Value: bson.A{"$failures", int64(1)}}}}}}},
		{Key: "windowStart", Value: bson.D{{Key: "$cond", Value: bson.A{expired, now, "$windowStart"}}}},
		{Key: "expiresAt", Value: now.Add(rule.Window)},
	}}}}
}

// Begin counts one attempt on every key before the attempt is evaluated and
// returns how long the caller must wait when a key's budget is already used
// up. The count is a single atomic document update, so concurrent attempts
// cannot all pass a read-only check before any of them is recorded: at most
// Limit attempts per window are ever evaluated. The caller resets the keys of
// a successful attempt (and forgives the ones that only count failures).
func (t *Throttle) Begin(ctx context.Context, keys []string) (time.Duration, error) {
	now := t.now()
	var wait time.Duration
	for _, key := range keys {
		rule := t.rule(key)
		var d throttleDoc
		err := t.store.col(colLoginThrottle).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: key}}, t.countPipeline(rule, now),
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&d)
		if err != nil {
			return 0, err
		}
		if d.Failures > int64(rule.Limit) {
			wait = max(wait, d.WindowStart.Add(rule.Window).Sub(now))
		}
	}
	if wait < 0 {
		wait = 0
	}
	return wait, nil
}

// Forgive takes back one attempt counted by Begin (for keys that count
// failures only, after the attempt succeeded).
func (t *Throttle) Forgive(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if _, err := t.store.col(colLoginThrottle).UpdateOne(ctx, bson.D{{Key: "_id", Value: key}, {Key: "failures", Value: bson.D{{Key: "$gt", Value: 0}}}},
			bson.D{{Key: "$inc", Value: bson.D{{Key: "failures", Value: int64(-1)}}}}); err != nil {
			return err
		}
	}
	return nil
}

// Reset clears the counters for keys (after a successful attempt).
func (t *Throttle) Reset(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := t.store.col(colLoginThrottle).DeleteMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: keys}}}})
	return err
}
