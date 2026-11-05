package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

type userDoc struct {
	Id                bson.ObjectID  `bson:"_id"`
	WorkspaceId       bson.ObjectID  `bson:"workspaceId"`
	Email             string         `bson:"email"`
	EmailNormalized   string         `bson:"emailNormalized"`
	Username          string         `bson:"username,omitempty"`
	DisplayName       string         `bson:"displayName"`
	GivenName         string         `bson:"givenName"`
	MiddleName        string         `bson:"middleName"`
	FamilyName        string         `bson:"familyName"`
	PasswordHash      string         `bson:"passwordHash,omitempty"`
	Status            string         `bson:"status"`
	Roles             []string       `bson:"roles"`
	Theme             string         `bson:"theme,omitempty"`
	EmailPrefs        *emailPrefsDoc `bson:"emailPrefs,omitempty"`
	Signature         string         `bson:"signature,omitempty"`
	EmailVerifiedAt   *time.Time     `bson:"emailVerifiedAt,omitempty"`
	PasswordChangedAt *time.Time     `bson:"passwordChangedAt,omitempty"`
	MFAEnabledAt      *time.Time     `bson:"mfaEnabledAt,omitempty"`
	CreatedAt         time.Time      `bson:"createdAt"`
	UpdatedAt         time.Time      `bson:"updatedAt"`
	Version           int64          `bson:"version"`
}

type emailPrefsDoc struct {
	MuteWatcherUpdates bool `bson:"muteWatcherUpdates"`
	MuteQueueUpdates   bool `bson:"muteQueueUpdates"`
}

func (d userDoc) toDomain() users.User {
	roles := d.Roles
	if roles == nil {
		roles = []string{}
	}
	return users.User{
		Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), Email: d.Email, EmailNormalized: d.EmailNormalized,
		Username: d.Username, Name: d.DisplayName, GivenName: d.GivenName, MiddleName: d.MiddleName, FamilyName: d.FamilyName, PasswordHash: d.PasswordHash, Status: d.Status, Roles: roles, Theme: d.Theme,
		PasswordChangedAt: d.PasswordChangedAt, MFAEnabledAt: d.MFAEnabledAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Version: d.Version,
		EmailPrefs: emailPrefsOf(d.EmailPrefs), Signature: d.Signature,
	}
}

func emailPrefsOf(d *emailPrefsDoc) users.EmailPreferences {
	if d == nil {
		return users.EmailPreferences{}
	}
	return users.EmailPreferences{MuteWatcherUpdates: d.MuteWatcherUpdates, MuteQueueUpdates: d.MuteQueueUpdates}
}

// UsersRepository implements users.Repository and auth.UserStore.
type UsersRepository struct{ store *Store }

// NewUsersRepository returns a users repository.
func NewUsersRepository(s *Store) *UsersRepository { return &UsersRepository{store: s} }

// Insert stores a new user with its audit event.
func (r *UsersRepository) Insert(ctx context.Context, u users.User, ev audit.Event) (users.User, error) {
	ws, err := oid(u.WorkspaceId)
	if err != nil {
		return users.User{}, err
	}
	doc := userDoc{Id: bson.NewObjectID(), WorkspaceId: ws, Email: u.Email, EmailNormalized: u.EmailNormalized,
		Username: u.Username, DisplayName: u.Name, GivenName: u.GivenName, MiddleName: u.MiddleName, FamilyName: u.FamilyName, PasswordHash: u.PasswordHash, Status: u.Status, Roles: u.Roles, Theme: u.Theme,
		PasswordChangedAt: u.PasswordChangedAt, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt, Version: 1}
	if doc.Roles == nil {
		doc.Roles = []string{}
	}
	if ev.Target == "" {
		ev.Target = "user/" + doc.Id.Hex()
	}
	err = r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colUsers).InsertOne(ctx, doc); err != nil {
			return userDuplicate(err)
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		return users.User{}, err
	}
	return doc.toDomain(), nil
}

// userDuplicate maps unique-index violations on users to domain errors.
func userDuplicate(err error) error {
	switch {
	case isDuplicate(err, "workspaceEmailUnique"):
		return users.ErrEmailTaken
	case isDuplicate(err, "workspaceUsernameUnique"):
		return users.ErrUsernameTaken
	}
	return err
}

func (r *UsersRepository) findOne(ctx context.Context, filter bson.D) (users.User, error) {
	var doc userDoc
	err := r.store.col(colUsers).FindOne(ctx, filter).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return users.User{}, users.ErrNotFound
	}
	if err != nil {
		return users.User{}, err
	}
	return doc.toDomain(), nil
}

// Get returns a user by ID within the workspace.
func (r *UsersRepository) Get(ctx context.Context, workspaceID, id string) (users.User, error) {
	ws, err1 := oid(workspaceID)
	uid, err2 := oid(id)
	if err1 != nil || err2 != nil {
		return users.User{}, users.ErrNotFound
	}
	return r.findOne(ctx, bson.D{{Key: "_id", Value: uid}, {Key: "workspaceId", Value: ws}})
}

// GetByEmail returns a user by normalized email within the workspace.
func (r *UsersRepository) GetByEmail(ctx context.Context, workspaceID, emailNormalized string) (users.User, error) {
	ws, err := oid(workspaceID)
	if err != nil {
		return users.User{}, users.ErrNotFound
	}
	return r.findOne(ctx, bson.D{{Key: "workspaceId", Value: ws}, {Key: "emailNormalized", Value: emailNormalized}})
}

// GetByUsername returns a user by normalized username within the workspace.
func (r *UsersRepository) GetByUsername(ctx context.Context, workspaceID, username string) (users.User, error) {
	ws, err := oid(workspaceID)
	if err != nil || username == "" {
		return users.User{}, users.ErrNotFound
	}
	return r.findOne(ctx, bson.D{{Key: "workspaceId", Value: ws}, {Key: "username", Value: username}})
}

// List pages through users ordered by normalized email.
func (r *UsersRepository) List(ctx context.Context, workspaceID string, after *pagination.Cursor, limit int) ([]users.User, error) {
	ws, err := oid(workspaceID)
	if err != nil {
		return nil, err
	}
	filter, err := pageFilter(bson.D{{Key: "workspaceId", Value: ws}}, "emailNormalized", after)
	if err != nil {
		return nil, err
	}
	docs, err := findAll[userDoc](ctx, r.store.col(colUsers), filter, pageOptions("emailNormalized", limit))
	if err != nil {
		return nil, err
	}
	out := make([]users.User, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// Apply writes a user change atomically with session revocation, the
// last-administrator guard and the audit event.
func (r *UsersRepository) Apply(ctx context.Context, c users.Change) (users.User, error) {
	u := c.User
	ws, err1 := oid(u.WorkspaceId)
	uid, err2 := oid(u.Id)
	if err1 != nil || err2 != nil {
		return users.User{}, users.ErrNotFound
	}
	var out users.User
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		filter := bson.D{{Key: "_id", Value: uid}, {Key: "workspaceId", Value: ws}}
		if c.ExpectedVersion > 0 {
			filter = append(filter, bson.E{Key: "version", Value: c.ExpectedVersion})
		}
		var set bson.D
		if c.Only == "" {
			set = append(nameSet(u), bson.E{Key: "status", Value: u.Status}, bson.E{Key: "roles", Value: u.Roles})
			if u.Username != "" {
				set = append(set, bson.E{Key: "username", Value: u.Username})
			}
		}
		if c.Only == users.OnlyProfile {
			set = nameSet(u)
		}
		if c.Only == "" || c.Only == users.OnlyTheme || c.Only == users.OnlyPreferences {
			set = append(set, bson.E{Key: "theme", Value: u.EffectiveTheme()})
		}
		if c.Only == users.OnlyPreferences {
			set = append(set, bson.E{Key: "signature", Value: u.Signature})
			set = append(set, bson.E{Key: "emailPrefs", Value: emailPrefsDoc{
				MuteWatcherUpdates: u.EmailPrefs.MuteWatcherUpdates, MuteQueueUpdates: u.EmailPrefs.MuteQueueUpdates}})
		}
		if c.Only == "" || c.Only == users.OnlyPassword {
			if u.PasswordHash != "" {
				set = append(set, bson.E{Key: "passwordHash", Value: u.PasswordHash})
			}
			if u.PasswordChangedAt != nil {
				set = append(set, bson.E{Key: "passwordChangedAt", Value: *u.PasswordChangedAt})
			}
		}
		if c.Only != users.OnlyTheme && c.Only != users.OnlyPreferences {
			set = append(set, bson.E{Key: "updatedAt", Value: u.UpdatedAt})
		}
		update := bson.D{{Key: "$set", Value: set}}
		if c.BumpVersion {
			update = append(update, bson.E{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}})
		}
		var doc userDoc
		err := r.store.col(colUsers).FindOneAndUpdate(ctx, filter, update,
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if mongo.IsDuplicateKeyError(err) {
			return userDuplicate(err)
		}
		if errors.Is(err, mongo.ErrNoDocuments) {
			if c.ExpectedVersion > 0 {
				if n, _ := r.store.col(colUsers).CountDocuments(ctx, bson.D{{Key: "_id", Value: uid}}); n > 0 {
					return users.ErrVersionConflict
				}
			}
			return users.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.GuardLastAdmin {
			// Write the workspace document first so that two guarded changes
			// (two administrators demoting or disabling each other at the
			// same moment) conflict and the loser recounts after a retry,
			// instead of both passing on their own snapshot (write skew).
			if _, err := r.store.col(colWorkspaces).UpdateOne(ctx, bson.D{{Key: "_id", Value: ws}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "adminGuardAt", Value: u.UpdatedAt}}}}); err != nil {
				return err
			}
			n, err := r.store.col(colUsers).CountDocuments(ctx, bson.D{{Key: "workspaceId", Value: ws},
				{Key: "roles", Value: users.RoleAdmin}, {Key: "status", Value: users.StatusActive}})
			if err != nil {
				return err
			}
			if n == 0 {
				return users.ErrLastAdmin
			}
		}
		if c.RevokeSessions {
			filter := bson.D{{Key: "userId", Value: uid}, {Key: "revokedAt", Value: nil}}
			if keep, err := oid(c.KeepSessionId); err == nil {
				filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$ne", Value: keep}}})
			}
			if _, err := r.store.col(colSessions).UpdateMany(ctx, filter, bson.D{{Key: "$set", Value: bson.D{
				{Key: "revokedAt", Value: u.UpdatedAt}, {Key: "revokedReason", Value: "accountChange"},
			}}}); err != nil {
				return err
			}
		}
		if c.Disabled {
			if err := r.store.insertAccessEvent(ctx, ws, outbox.UserDisabled, nil, uid, nil, c.Event, u.UpdatedAt); err != nil {
				return err
			}
		}
		if c.Event != nil {
			ev := *c.Event
			ev.WorkspaceId = u.WorkspaceId
			if ev.Target == "" {
				ev.Target = "user/" + u.Id
			}
			if err := r.store.insertAudit(ctx, ws, ev); err != nil {
				return err
			}
		}
		out = doc.toDomain()
		return nil
	})
	return out, err
}

// nameSet writes the name parts and the derived display name.
func nameSet(u users.User) bson.D {
	return bson.D{{Key: "displayName", Value: u.Name}, {Key: "givenName", Value: u.GivenName},
		{Key: "middleName", Value: u.MiddleName}, {Key: "familyName", Value: u.FamilyName}}
}
