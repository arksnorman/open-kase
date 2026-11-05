package mongo

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/settings"
)

// SettingsRepository implements settings.Repository.
type SettingsRepository struct{ store *Store }

// NewSettingsRepository returns a settings repository.
func NewSettingsRepository(s *Store) *SettingsRepository { return &SettingsRepository{store: s} }

func (r *SettingsRepository) find(ctx context.Context) (settingsDoc, error) {
	var doc settingsDoc
	err := r.store.col(colSettings).FindOne(ctx, bson.D{{Key: "scope", Value: "installation"}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return doc, settings.ErrNotInitialized
	}
	return doc, err
}

// Load returns the current installation settings.
func (r *SettingsRepository) Load(ctx context.Context) (settings.Record, error) {
	doc, err := r.find(ctx)
	if err != nil {
		return settings.Record{}, err
	}
	return settings.Record{
		WorkspaceId: doc.WorkspaceId.Hex(), Version: doc.ConfigVersion,
		Settings: doc.Data.toDomain(), UpdatedAt: doc.UpdatedAt,
	}, nil
}

// Version returns only the current config_version (cheap polling).
func (r *SettingsRepository) Version(ctx context.Context) (int64, error) {
	doc, err := r.find(ctx)
	return doc.ConfigVersion, err
}

// Replace writes the next version only if the stored version still equals
// expectedVersion, and appends the audit event in the same transaction.
func (r *SettingsRepository) Replace(ctx context.Context, expectedVersion int64, next settings.Record, event audit.Event) error {
	wsID, err := bson.ObjectIDFromHex(next.WorkspaceId)
	if err != nil {
		return err
	}
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colSettings).UpdateOne(ctx,
			bson.D{{Key: "workspaceId", Value: wsID}, {Key: "scope", Value: "installation"}, {Key: "configVersion", Value: expectedVersion}},
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "configVersion", Value: next.Version},
				{Key: "data", Value: toSettingsData(next.Settings)},
				{Key: "updatedAt", Value: next.UpdatedAt},
				{Key: "updatedBy", Value: actorDoc{Type: event.Actor.Type, Id: event.Actor.Id, Label: event.Actor.Label}},
			}}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return settings.ErrVersionConflict
		}
		_, err = r.store.col(colAuditEvents).InsertOne(ctx, toAuditDoc(wsID, event))
		return err
	})
}
