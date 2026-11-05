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
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/users"
)

// IdentityRepository implements auth.MFARepository,
// auth.AccountTokenRepository and notifications.Accounts.
type IdentityRepository struct {
	store *Store
	jobs  *JobsRepository
}

// NewIdentityRepository returns the account-security repository.
func NewIdentityRepository(s *Store, js *JobsRepository) *IdentityRepository {
	return &IdentityRepository{store: s, jobs: js}
}

// ---- MFA ----

type userMFADoc struct {
	Id                bson.ObjectID `bson:"_id"`
	WorkspaceId       bson.ObjectID `bson:"workspaceId"`
	SecretCiphertext  string        `bson:"secretCiphertext,omitempty"`
	PendingCiphertext string        `bson:"pendingCiphertext,omitempty"`
	PendingExpiresAt  *time.Time    `bson:"pendingExpiresAt,omitempty"`
	EnabledAt         *time.Time    `bson:"enabledAt,omitempty"`
	LastStep          int64         `bson:"lastStep"`
	RecoveryHashes    []string      `bson:"recoveryHashes"`
	UpdatedAt         time.Time     `bson:"updatedAt"`
}

func mfaKey(workspaceID, userID string) (bson.D, bool) {
	ws, err1 := oid(workspaceID)
	uid, err2 := oid(userID)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	return bson.D{{Key: "_id", Value: uid}, {Key: "workspaceId", Value: ws}}, true
}

// GetMFA returns a user's enrolment.
func (r *IdentityRepository) GetMFA(ctx context.Context, workspaceID, userID string) (auth.MFAState, error) {
	filter, ok := mfaKey(workspaceID, userID)
	if !ok {
		return auth.MFAState{}, auth.ErrMFANotFound
	}
	var d userMFADoc
	err := r.store.col(colUserMFA).FindOne(ctx, filter).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.MFAState{}, auth.ErrMFANotFound
	}
	if err != nil {
		return auth.MFAState{}, err
	}
	return auth.MFAState{UserId: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), SecretCiphertext: d.SecretCiphertext,
		PendingCiphertext: d.PendingCiphertext, PendingExpiresAt: d.PendingExpiresAt, EnabledAt: d.EnabledAt,
		LastStep: d.LastStep, RecoveryHashes: d.RecoveryHashes, UpdatedAt: d.UpdatedAt}, nil
}

// SavePendingMFA stores an unconfirmed secret (upsert) with its audit event.
func (r *IdentityRepository) SavePendingMFA(ctx context.Context, st auth.MFAState, ev audit.Event) error {
	filter, ok := mfaKey(st.WorkspaceId, st.UserId)
	if !ok {
		return auth.ErrMFANotFound
	}
	ws := filter[1].Value.(bson.ObjectID)
	return r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colUserMFA).UpdateOne(ctx, filter, bson.D{
			{Key: "$set", Value: bson.D{{Key: "pendingCiphertext", Value: st.PendingCiphertext},
				{Key: "pendingExpiresAt", Value: st.PendingExpiresAt}, {Key: "updatedAt", Value: st.UpdatedAt}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "lastStep", Value: int64(0)}, {Key: "recoveryHashes", Value: bson.A{}}}},
		}, options.UpdateOne().SetUpsert(true)); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
}

// EnableMFA confirms the pending secret and marks the user enrolled.
func (r *IdentityRepository) EnableMFA(ctx context.Context, workspaceID, userID, pending string, step int64, hashes []string,
	keepSessionID string, now time.Time, ev audit.Event) error {
	filter, ok := mfaKey(workspaceID, userID)
	if !ok {
		return auth.ErrMFANotFound
	}
	uid, ws := filter[0].Value.(bson.ObjectID), filter[1].Value.(bson.ObjectID)
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colUserMFA).UpdateOne(ctx, append(filter, bson.E{Key: "pendingCiphertext", Value: pending}), bson.D{
			{Key: "$set", Value: bson.D{{Key: "secretCiphertext", Value: pending}, {Key: "enabledAt", Value: now},
				{Key: "lastStep", Value: step}, {Key: "recoveryHashes", Value: hashes}, {Key: "updatedAt", Value: now}}},
			{Key: "$unset", Value: bson.D{{Key: "pendingCiphertext", Value: ""}, {Key: "pendingExpiresAt", Value: ""}}},
		})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return auth.ErrMFANotFound
		}
		if res, err := r.store.col(colUsers).UpdateOne(ctx, bson.D{{Key: "_id", Value: uid}, {Key: "workspaceId", Value: ws}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "mfaEnabledAt", Value: now}}}}); err != nil {
			return err
		} else if res.MatchedCount == 0 {
			return users.ErrNotFound
		}
		revoke := bson.D{{Key: "userId", Value: uid}, {Key: "revokedAt", Value: nil}}
		if keep, err := oid(keepSessionID); err == nil {
			revoke = append(revoke, bson.E{Key: "_id", Value: bson.D{{Key: "$ne", Value: keep}}})
		}
		if _, err := r.store.col(colSessions).UpdateMany(ctx, revoke, bson.D{{Key: "$set", Value: bson.D{
			{Key: "revokedAt", Value: now}, {Key: "revokedReason", Value: "mfaEnabled"}}}}); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
}

// DisableMFA removes the enrolment and clears the user's flag.
func (r *IdentityRepository) DisableMFA(ctx context.Context, workspaceID, userID string, _ time.Time, ev audit.Event) error {
	filter, ok := mfaKey(workspaceID, userID)
	if !ok {
		return auth.ErrMFANotFound
	}
	uid, ws := filter[0].Value.(bson.ObjectID), filter[1].Value.(bson.ObjectID)
	return r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colUserMFA).DeleteOne(ctx, filter); err != nil {
			return err
		}
		if _, err := r.store.col(colUsers).UpdateOne(ctx, bson.D{{Key: "_id", Value: uid}, {Key: "workspaceId", Value: ws}},
			bson.D{{Key: "$unset", Value: bson.D{{Key: "mfaEnabledAt", Value: ""}}}}); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
}

// ReplaceRecoveryCodes stores new recovery digests.
func (r *IdentityRepository) ReplaceRecoveryCodes(ctx context.Context, workspaceID, userID string, hashes []string, now time.Time, ev audit.Event) error {
	filter, ok := mfaKey(workspaceID, userID)
	if !ok {
		return auth.ErrMFANotFound
	}
	ws := filter[1].Value.(bson.ObjectID)
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colUserMFA).UpdateOne(ctx, append(filter, bson.E{Key: "enabledAt", Value: bson.D{{Key: "$ne", Value: nil}}}),
			bson.D{{Key: "$set", Value: bson.D{{Key: "recoveryHashes", Value: hashes}, {Key: "updatedAt", Value: now}}}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return auth.ErrMFANotFound
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
}

// UseRecoveryCode removes hash in one atomic update, so a code can be
// redeemed only once even under concurrent sign-ins.
func (r *IdentityRepository) UseRecoveryCode(ctx context.Context, workspaceID, userID, hash string, now time.Time) (bool, error) {
	filter, ok := mfaKey(workspaceID, userID)
	if !ok {
		return false, nil
	}
	res, err := r.store.col(colUserMFA).UpdateOne(ctx, append(filter,
		bson.E{Key: "enabledAt", Value: bson.D{{Key: "$ne", Value: nil}}}, bson.E{Key: "recoveryHashes", Value: hash}),
		bson.D{{Key: "$pull", Value: bson.D{{Key: "recoveryHashes", Value: hash}}}, {Key: "$set", Value: bson.D{{Key: "updatedAt", Value: now}}}})
	if err != nil {
		return false, err
	}
	return res.ModifiedCount == 1, nil
}

// AdvanceStep records a used TOTP step; a code is accepted at most once.
func (r *IdentityRepository) AdvanceStep(ctx context.Context, workspaceID, userID string, step int64) (bool, error) {
	filter, ok := mfaKey(workspaceID, userID)
	if !ok {
		return false, nil
	}
	res, err := r.store.col(colUserMFA).UpdateOne(ctx, append(filter, bson.E{Key: "lastStep", Value: bson.D{{Key: "$lt", Value: step}}}),
		bson.D{{Key: "$set", Value: bson.D{{Key: "lastStep", Value: step}}}})
	if err != nil {
		return false, err
	}
	return res.ModifiedCount == 1, nil
}

type mfaChallengeDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	UserId      bson.ObjectID `bson:"userId"`
	TokenHash   string        `bson:"tokenHash"`
	Attempts    int32         `bson:"attempts"`
	CreatedAt   time.Time     `bson:"createdAt"`
	ExpiresAt   time.Time     `bson:"expiresAt"`
	UsedAt      *time.Time    `bson:"usedAt"`
	UserAgent   string        `bson:"userAgent"`
	ClientIP    string        `bson:"clientIp"`
}

// CreateChallenge stores a sign-in challenge.
func (r *IdentityRepository) CreateChallenge(ctx context.Context, ch auth.MFAChallenge) (auth.MFAChallenge, error) {
	ws, err1 := oid(ch.WorkspaceId)
	uid, err2 := oid(ch.UserId)
	if err1 != nil || err2 != nil {
		return auth.MFAChallenge{}, errBadID
	}
	d := mfaChallengeDoc{Id: bson.NewObjectID(), WorkspaceId: ws, UserId: uid, TokenHash: ch.TokenHash, CreatedAt: ch.CreatedAt,
		ExpiresAt: ch.ExpiresAt, UserAgent: ch.UserAgent, ClientIP: ch.ClientIP}
	if _, err := r.store.col(colMFAChallenges).InsertOne(ctx, d); err != nil {
		return auth.MFAChallenge{}, err
	}
	ch.Id = d.Id.Hex()
	return ch, nil
}

// ChallengeByHash finds a challenge by its token digest.
func (r *IdentityRepository) ChallengeByHash(ctx context.Context, hash string) (auth.MFAChallenge, error) {
	var d mfaChallengeDoc
	err := r.store.col(colMFAChallenges).FindOne(ctx, bson.D{{Key: "tokenHash", Value: hash}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.MFAChallenge{}, auth.ErrChallengeNotFound
	}
	if err != nil {
		return auth.MFAChallenge{}, err
	}
	return auth.MFAChallenge{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), UserId: d.UserId.Hex(), TokenHash: d.TokenHash,
		Attempts: int(d.Attempts), CreatedAt: d.CreatedAt, ExpiresAt: d.ExpiresAt, UsedAt: d.UsedAt, UserAgent: d.UserAgent, ClientIP: d.ClientIP}, nil
}

// ChallengeAttempt increments the attempt counter and returns it.
func (r *IdentityRepository) ChallengeAttempt(ctx context.Context, id string) (int, error) {
	var d mfaChallengeDoc
	err := r.store.col(colMFAChallenges).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: mustOID(id)}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "attempts", Value: int32(1)}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, auth.ErrChallengeNotFound
	}
	return int(d.Attempts), err
}

// ConsumeChallenge marks an unused challenge used.
func (r *IdentityRepository) ConsumeChallenge(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := r.store.col(colMFAChallenges).UpdateOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "usedAt", Value: nil}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "usedAt", Value: now}}}})
	if err != nil {
		return false, err
	}
	return res.ModifiedCount == 1, nil
}

// ---- Account tokens ----

type accountTokenDoc struct {
	Id          bson.ObjectID  `bson:"_id"`
	WorkspaceId bson.ObjectID  `bson:"workspaceId"`
	UserId      bson.ObjectID  `bson:"userId"`
	Purpose     string         `bson:"purpose"`
	TokenHash   string         `bson:"tokenHash"`
	CreatedAt   time.Time      `bson:"createdAt"`
	ExpiresAt   time.Time      `bson:"expiresAt"`
	UsedAt      *time.Time     `bson:"usedAt"`
	RevokedAt   *time.Time     `bson:"revokedAt"`
	CreatedBy   actorDoc       `bson:"createdBy"`
	DeliveryId  *bson.ObjectID `bson:"deliveryId"`
}

func (d accountTokenDoc) toDomain() auth.AccountToken {
	return auth.AccountToken{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), UserId: d.UserId.Hex(), Purpose: d.Purpose,
		TokenHash: d.TokenHash, CreatedAt: d.CreatedAt, ExpiresAt: d.ExpiresAt, UsedAt: d.UsedAt, RevokedAt: d.RevokedAt,
		CreatedBy: audit.Actor{Type: d.CreatedBy.Type, Id: d.CreatedBy.Id, Label: d.CreatedBy.Label}, DeliveryId: hexOrEmpty(d.DeliveryId)}
}

// unusedTokens matches a user's redeemable-looking tokens of purpose.
func unusedTokens(uid bson.ObjectID, purpose string) bson.D {
	return bson.D{{Key: "userId", Value: uid}, {Key: "purpose", Value: purpose}, {Key: "usedAt", Value: nil}, {Key: "revokedAt", Value: nil}}
}

// IssueAccountToken stores a token, its email job and delivery, revoking
// earlier unused tokens of the same purpose, in one transaction.
func (r *IdentityRepository) IssueAccountToken(ctx context.Context, tok auth.AccountToken, build func(string) (auth.AccountMail, error), ev audit.Event) (auth.AccountToken, error) {
	ws, err1 := oid(tok.WorkspaceId)
	uid, err2 := oid(tok.UserId)
	if err1 != nil || err2 != nil {
		return auth.AccountToken{}, users.ErrNotFound
	}
	var out accountTokenDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colAccountTokens).UpdateMany(ctx, unusedTokens(uid, tok.Purpose),
			bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: tok.CreatedAt}}}}); err != nil {
			return err
		}
		d := accountTokenDoc{Id: bson.NewObjectID(), WorkspaceId: ws, UserId: uid, Purpose: tok.Purpose, TokenHash: tok.TokenHash,
			CreatedAt: tok.CreatedAt, ExpiresAt: tok.ExpiresAt,
			CreatedBy: actorDoc{Type: tok.CreatedBy.Type, Id: tok.CreatedBy.Id, Label: tok.CreatedBy.Label}}
		mail, err := build(d.Id.Hex())
		if err != nil {
			return err
		}
		jobID, ok, err := r.jobs.enqueueOne(ctx, jobs.NewJob{WorkspaceId: tok.WorkspaceId, Kind: notifications.KindAccountEmail,
			Key: "account-email:" + d.Id.Hex(), Payload: map[string]string{"tokenId": d.Id.Hex(), "userId": tok.UserId,
				"purpose": tok.Purpose, "tokenSealed": mail.TokenSealed}})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("identity: account email job already exists")
		}
		del := newDeliveryDoc(mustOID(jobID), notifications.Delivery{WorkspaceId: tok.WorkspaceId, Channel: notifications.ChannelEmail,
			Recipient: notifications.Recipient{Kind: notifications.RecipientUser, Id: tok.UserId, Address: mail.Address},
			Reason:    notifications.ReasonAccount, Template: mail.Template, EventType: "account." + tok.Purpose}, r.jobs.now())
		if _, err := r.store.col(colDeliveries).InsertOne(ctx, del); err != nil {
			return err
		}
		d.DeliveryId = &del.Id
		if _, err := r.store.col(colAccountTokens).InsertOne(ctx, d); err != nil {
			return err
		}
		out = d
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		return auth.AccountToken{}, err
	}
	return out.toDomain(), nil
}

// RedeemAccountToken uses a token and applies the account change.
func (r *IdentityRepository) RedeemAccountToken(ctx context.Context, hash, purpose string, now time.Time, c auth.AccountCompletion, ev audit.Event) (string, error) {
	var userID string
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		var tok accountTokenDoc
		err := r.store.col(colAccountTokens).FindOneAndUpdate(ctx, bson.D{{Key: "tokenHash", Value: hash}, {Key: "purpose", Value: purpose},
			{Key: "usedAt", Value: nil}, {Key: "revokedAt", Value: nil}, {Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "usedAt", Value: now}}}}).Decode(&tok)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return auth.ErrAccountTokenInvalid
		}
		if err != nil {
			return err
		}
		userFilter := bson.D{{Key: "_id", Value: tok.UserId}, {Key: "workspaceId", Value: tok.WorkspaceId}, {Key: "status", Value: c.RequireStatus}}
		set := bson.D{{Key: "passwordHash", Value: c.PasswordHash}, {Key: "passwordChangedAt", Value: now}, {Key: "updatedAt", Value: now}}
		if c.Activate {
			set = append(set, bson.E{Key: "status", Value: users.StatusActive}, bson.E{Key: "emailVerifiedAt", Value: now})
		}
		if c.Name != nil {
			set = append(set, bson.E{Key: "displayName", Value: c.Name.Full()}, bson.E{Key: "givenName", Value: c.Name.Given},
				bson.E{Key: "middleName", Value: c.Name.Middle}, bson.E{Key: "familyName", Value: c.Name.Family})
		}
		res, err := r.store.col(colUsers).UpdateOne(ctx, userFilter, bson.D{{Key: "$set", Value: set},
			{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return auth.ErrAccountStateMismatch
		}
		if _, err := r.store.col(colSessions).UpdateMany(ctx, bson.D{{Key: "userId", Value: tok.UserId}, {Key: "revokedAt", Value: nil}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: now}, {Key: "revokedReason", Value: purpose}}}}); err != nil {
			return err
		}
		// Any other outstanding link of the same purpose is now pointless.
		if _, err := r.store.col(colAccountTokens).UpdateMany(ctx, unusedTokens(tok.UserId, purpose),
			bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: now}}}}); err != nil {
			return err
		}
		userID = tok.UserId.Hex()
		ev.WorkspaceId = tok.WorkspaceId.Hex()
		ev.Actor.Id, ev.Target = userID, "user/"+userID
		return r.store.insertAudit(ctx, tok.WorkspaceId, ev)
	})
	return userID, err
}

// RevokeAccountTokens revokes a user's unused tokens of purpose.
func (r *IdentityRepository) RevokeAccountTokens(ctx context.Context, workspaceID, userID, purpose string, now time.Time, ev audit.Event) (int, error) {
	ws, err1 := oid(workspaceID)
	uid, err2 := oid(userID)
	if err1 != nil || err2 != nil {
		return 0, nil
	}
	var n int
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		filter := append(unusedTokens(uid, purpose), bson.E{Key: "workspaceId", Value: ws}, bson.E{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}})
		res, err := r.store.col(colAccountTokens).UpdateMany(ctx, filter, bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: now}}}})
		if err != nil {
			return err
		}
		n = int(res.ModifiedCount)
		if n == 0 {
			return nil
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	return n, err
}

// PendingAccountToken returns the newest redeemable token.
// AccountTokenUsable implements auth.AccountTokenRepository.
func (r *IdentityRepository) AccountTokenUsable(ctx context.Context, hash, purpose string, now time.Time) (bool, error) {
	n, err := r.store.col(colAccountTokens).CountDocuments(ctx, bson.D{{Key: "tokenHash", Value: hash}, {Key: "purpose", Value: purpose},
		{Key: "usedAt", Value: nil}, {Key: "revokedAt", Value: nil}, {Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}}},
		options.Count().SetLimit(1))
	return n > 0, err
}

func (r *IdentityRepository) PendingAccountToken(ctx context.Context, workspaceID, userID, purpose string, now time.Time) (auth.AccountToken, error) {
	ws, err1 := oid(workspaceID)
	uid, err2 := oid(userID)
	if err1 != nil || err2 != nil {
		return auth.AccountToken{}, auth.ErrAccountTokenInvalid
	}
	var d accountTokenDoc
	err := r.store.col(colAccountTokens).FindOne(ctx, append(unusedTokens(uid, purpose), bson.E{Key: "workspaceId", Value: ws},
		bson.E{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}}),
		options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.AccountToken{}, auth.ErrAccountTokenInvalid
	}
	if err != nil {
		return auth.AccountToken{}, err
	}
	return d.toDomain(), nil
}

// AccountTokenInfo implements notifications.Accounts for the email worker.
func (r *IdentityRepository) AccountTokenInfo(ctx context.Context, tokenID string, now time.Time) (notifications.AccountTokenInfo, error) {
	id, err := oid(tokenID)
	if err != nil {
		return notifications.AccountTokenInfo{}, notifications.ErrNotFound
	}
	var d accountTokenDoc
	err = r.store.col(colAccountTokens).FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return notifications.AccountTokenInfo{}, notifications.ErrNotFound
	}
	if err != nil {
		return notifications.AccountTokenInfo{}, err
	}
	var u userDoc
	err = r.store.col(colUsers).FindOne(ctx, bson.D{{Key: "_id", Value: d.UserId}, {Key: "workspaceId", Value: d.WorkspaceId}}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return notifications.AccountTokenInfo{}, notifications.ErrNotFound
	}
	if err != nil {
		return notifications.AccountTokenInfo{}, err
	}
	wantStatus := users.StatusActive
	if d.Purpose == auth.PurposeInvitation {
		wantStatus = users.StatusInvited
	}
	info := notifications.AccountTokenInfo{Purpose: d.Purpose, ExpiresAt: d.ExpiresAt, UserEmail: u.Email, UserName: u.DisplayName,
		Valid: d.UsedAt == nil && d.RevokedAt == nil && now.Before(d.ExpiresAt) && u.Status == wantStatus}
	if d.Purpose == auth.PurposeInvitation && d.CreatedBy.Type == audit.ActorUser {
		var inviter userDoc
		if err := r.store.col(colUsers).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(d.CreatedBy.Id)}}).Decode(&inviter); err == nil {
			info.InvitedBy = inviter.DisplayName
		}
	}
	return info, nil
}

// ---- Signing key rotation ----

// RotateKey retires the active key and inserts the next one atomically.
func (r *SigningKeysRepository) RotateKey(ctx context.Context, workspaceID, oldID string, next auth.StoredKey, now, verifyUntil time.Time, ev audit.Event) error {
	ws, err := oid(workspaceID)
	if err != nil {
		return auth.ErrKeyNotFound
	}
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colSigningKeys).UpdateOne(ctx, bson.D{{Key: "_id", Value: oldID}, {Key: "workspaceId", Value: ws}, {Key: "active", Value: true}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}, {Key: "retiredAt", Value: now}, {Key: "verifyUntil", Value: verifyUntil}}}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return auth.ErrKeyNotFound
		}
		if _, err := r.store.col(colSigningKeys).InsertOne(ctx, signingKeyDoc{Id: next.Id, WorkspaceId: ws, Algorithm: next.Algorithm,
			PublicKey: next.PublicKey, PrivateCiphertext: next.PrivateCiphertext, Active: true, CreatedAt: next.CreatedAt}); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
}
