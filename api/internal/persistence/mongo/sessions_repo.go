package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/auth"
)

type sessionDoc struct {
	Id            bson.ObjectID `bson:"_id"`
	WorkspaceId   bson.ObjectID `bson:"workspaceId"`
	UserId        bson.ObjectID `bson:"userId"`
	CSRFHash      string        `bson:"csrfHash"`
	CreatedAt     time.Time     `bson:"createdAt"`
	LastUsedAt    time.Time     `bson:"lastUsedAt"`
	ExpiresAt     time.Time     `bson:"expiresAt"`
	RevokedAt     *time.Time    `bson:"revokedAt"`
	RevokedReason string        `bson:"revokedReason,omitempty"`
	UserAgent     string        `bson:"userAgent,omitempty"`
	ClientIP      string        `bson:"clientIp,omitempty"`
}

func (d sessionDoc) toDomain() auth.Session {
	return auth.Session{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), UserId: d.UserId.Hex(), CSRFHash: d.CSRFHash,
		CreatedAt: d.CreatedAt, LastUsedAt: d.LastUsedAt, ExpiresAt: d.ExpiresAt, RevokedAt: d.RevokedAt,
		RevokedReason: d.RevokedReason, UserAgent: d.UserAgent, ClientIP: d.ClientIP}
}

type refreshTokenDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	SessionId   bson.ObjectID `bson:"sessionId"`
	TokenHash   string        `bson:"tokenHash"`
	CreatedAt   time.Time     `bson:"createdAt"`
	ExpiresAt   time.Time     `bson:"expiresAt"`
	UsedAt      *time.Time    `bson:"usedAt"`
	// SuccessorID is the token this one was rotated into (set with UsedAt).
	SuccessorId *bson.ObjectID `bson:"successorId,omitempty"`
}

// SessionsRepository implements auth.SessionRepository.
type SessionsRepository struct{ store *Store }

// NewSessionsRepository returns a sessions repository.
func NewSessionsRepository(s *Store) *SessionsRepository { return &SessionsRepository{store: s} }

// CreateSession stores a session, its first refresh token and the login event.
func (r *SessionsRepository) CreateSession(ctx context.Context, s auth.Session, refreshHash string, ev audit.Event) (auth.Session, error) {
	ws, err1 := oid(s.WorkspaceId)
	uid, err2 := oid(s.UserId)
	if err1 != nil || err2 != nil {
		return auth.Session{}, errBadID
	}
	doc := sessionDoc{Id: bson.NewObjectID(), WorkspaceId: ws, UserId: uid, CSRFHash: s.CSRFHash, CreatedAt: s.CreatedAt,
		LastUsedAt: s.LastUsedAt, ExpiresAt: s.ExpiresAt, UserAgent: s.UserAgent, ClientIP: s.ClientIP}
	tok := refreshTokenDoc{Id: bson.NewObjectID(), WorkspaceId: ws, SessionId: doc.Id, TokenHash: refreshHash,
		CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt}
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colSessions).InsertOne(ctx, doc); err != nil {
			return err
		}
		if _, err := r.store.col(colRefreshTokens).InsertOne(ctx, tok); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		return auth.Session{}, err
	}
	return doc.toDomain(), nil
}

func (r *SessionsRepository) sessionByID(ctx context.Context, id bson.ObjectID) (sessionDoc, error) {
	var doc sessionDoc
	err := r.store.col(colSessions).FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return doc, auth.ErrSessionNotFound
	}
	return doc, err
}

// SessionByRefresh finds the session owning a refresh-token hash.
func (r *SessionsRepository) SessionByRefresh(ctx context.Context, refreshHash string) (auth.Session, error) {
	var tok refreshTokenDoc
	err := r.store.col(colRefreshTokens).FindOne(ctx, bson.D{{Key: "tokenHash", Value: refreshHash}}).Decode(&tok)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	if err != nil {
		return auth.Session{}, err
	}
	doc, err := r.sessionByID(ctx, tok.SessionId)
	if err != nil {
		return auth.Session{}, err
	}
	return doc.toDomain(), nil
}

// GetSession returns a session by ID.
func (r *SessionsRepository) GetSession(ctx context.Context, id string) (auth.Session, error) {
	sid, err := oid(id)
	if err != nil {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	doc, err := r.sessionByID(ctx, sid)
	if err != nil {
		return auth.Session{}, err
	}
	return doc.toDomain(), nil
}

// RotateRefresh marks the presented token used and stores its successor in
// one transaction. Presenting a used token revokes the session (the token
// family) and records the reuse, unless the token was rotated at most grace
// ago and its successor has not been used: that is a second browser tab
// presenting the shared cookie, and the outcome is auth.Replayed with no
// write at all. Concurrent presentations of the same token conflict on the
// token document; the driver retries the loser, which then sees the token as
// used.
func (r *SessionsRepository) RotateRefresh(ctx context.Context, presentedHash, newHash string, now time.Time, grace time.Duration, reuseEvent audit.Event) (auth.Session, auth.RotateOutcome, error) {
	var (
		out     auth.Session
		outcome auth.RotateOutcome
	)
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		var tok refreshTokenDoc
		err := r.store.col(colRefreshTokens).FindOne(ctx, bson.D{{Key: "tokenHash", Value: presentedHash}}).Decode(&tok)
		if errors.Is(err, mongo.ErrNoDocuments) {
			outcome = auth.Invalid
			return nil
		}
		if err != nil {
			return err
		}
		sess, err := r.sessionByID(ctx, tok.SessionId)
		if errors.Is(err, auth.ErrSessionNotFound) {
			outcome = auth.Invalid
			return nil
		}
		if err != nil {
			return err
		}
		out = sess.toDomain()
		if !out.Valid(now) {
			outcome = auth.Invalid
			return nil
		}
		if tok.UsedAt != nil {
			replayed, err := r.withinGrace(ctx, tok, now, grace)
			if err != nil {
				return err
			}
			if replayed {
				outcome = auth.Replayed
				return nil
			}
			if _, err := r.store.col(colSessions).UpdateOne(ctx, bson.D{{Key: "_id", Value: sess.Id}, {Key: "revokedAt", Value: nil}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: now}, {Key: "revokedReason", Value: "refreshTokenReuse"}}}}); err != nil {
				return err
			}
			ev := reuseEvent
			ev.Target = "session/" + sess.Id.Hex()
			ev.Actor = audit.Actor{Type: audit.ActorUser, Id: sess.UserId.Hex()}
			if err := r.store.insertAudit(ctx, sess.WorkspaceId, ev); err != nil {
				return err
			}
			revokedAt := now
			out.RevokedAt = &revokedAt
			outcome = auth.Reused
			return nil
		}
		nextID := bson.NewObjectID()
		res, err := r.store.col(colRefreshTokens).UpdateOne(ctx, bson.D{{Key: "_id", Value: tok.Id}, {Key: "usedAt", Value: nil}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "usedAt", Value: now}, {Key: "successorId", Value: nextID}}}})
		if err != nil {
			return err
		}
		if res.ModifiedCount != 1 {
			// Cannot happen inside a snapshot transaction; fail safe.
			outcome = auth.Invalid
			return nil
		}
		if _, err := r.store.col(colRefreshTokens).InsertOne(ctx, refreshTokenDoc{Id: nextID, WorkspaceId: sess.WorkspaceId,
			SessionId: sess.Id, TokenHash: newHash, CreatedAt: now, ExpiresAt: sess.ExpiresAt}); err != nil {
			return err
		}
		if _, err := r.store.col(colSessions).UpdateOne(ctx, bson.D{{Key: "_id", Value: sess.Id}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "lastUsedAt", Value: now}}}}); err != nil {
			return err
		}
		out.LastUsedAt = now
		outcome = auth.Rotated
		return nil
	})
	return out, outcome, err
}

// withinGrace reports whether a used token is the immediately previous token
// of its family presented again inside the grace window: it was rotated at
// most grace ago and the token it was rotated into is still unused. Tokens
// rotated before successor links were recorded never qualify.
//
// now is taken by the caller before its transaction starts, so the request
// that lost a concurrent rotation can carry a now slightly earlier than the
// winner's used_at; that is inside the window too.
func (r *SessionsRepository) withinGrace(ctx context.Context, tok refreshTokenDoc, now time.Time, grace time.Duration) (bool, error) {
	if grace <= 0 || tok.UsedAt == nil || tok.SuccessorId == nil || now.Sub(*tok.UsedAt) > grace {
		return false, nil
	}
	var next refreshTokenDoc
	err := r.store.col(colRefreshTokens).FindOne(ctx, bson.D{{Key: "_id", Value: *tok.SuccessorId}, {Key: "sessionId", Value: tok.SessionId}}).Decode(&next)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return next.UsedAt == nil, nil
}

// RevokeSession revokes a session (idempotent) and appends ev.
func (r *SessionsRepository) RevokeSession(ctx context.Context, id, reason string, now time.Time, ev audit.Event) error {
	sid, err := oid(id)
	if err != nil {
		return auth.ErrSessionNotFound
	}
	return r.store.transaction(ctx, func(ctx context.Context) error {
		var doc sessionDoc
		err := r.store.col(colSessions).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: sid}, {Key: "revokedAt", Value: nil}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: now}, {Key: "revokedReason", Value: reason}}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil // already revoked
		}
		if err != nil {
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
}
