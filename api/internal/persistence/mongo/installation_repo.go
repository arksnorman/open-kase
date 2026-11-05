package mongo

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/installation"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/users"
)

const installKey = "primary"

// InstallationRepository implements installation.Repository.
type InstallationRepository struct{ store *Store }

// NewInstallationRepository returns an installation repository.
func NewInstallationRepository(s *Store) *InstallationRepository {
	return &InstallationRepository{store: s}
}

// State reports whether the workspace and an administrator exist.
func (r *InstallationRepository) State(ctx context.Context) (installation.State, error) {
	var ws workspaceDoc
	err := r.store.col(colWorkspaces).FindOne(ctx, bson.D{{Key: "installKey", Value: installKey}}).Decode(&ws)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return installation.State{}, nil
	}
	if err != nil {
		return installation.State{}, err
	}
	return installation.State{
		Initialized: true, WorkspaceId: ws.Id.Hex(), WorkspaceName: ws.Name,
		KeyCheck: ws.KeyCheck, AdminExists: ws.BootstrapAdminId != nil,
	}, nil
}

// Seed inserts every missing installation record in one transaction. The
// workspace document is written by every attempt, so concurrent
// initializations conflict and the driver retries the loser, which then
// observes the winner's records instead of duplicating them.
func (r *InstallationRepository) Seed(ctx context.Context, seed installation.Seed) (installation.Result, error) {
	var res installation.Result
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		res = installation.Result{}
		now := seed.Now

		// Workspace (unique install_key).
		up, err := r.store.col(colWorkspaces).UpdateOne(ctx,
			bson.D{{Key: "installKey", Value: installKey}},
			bson.D{
				{Key: "$setOnInsert", Value: bson.D{
					{Key: "_id", Value: bson.NewObjectID()}, {Key: "name", Value: seed.WorkspaceName},
					{Key: "keyCheck", Value: seed.KeyCheck}, {Key: "bootstrapAdminId", Value: nil},
					{Key: "createdAt", Value: now},
				}},
				{Key: "$set", Value: bson.D{{Key: "updatedAt", Value: now}}},
			},
			options.UpdateOne().SetUpsert(true))
		if err != nil {
			return err
		}
		var ws workspaceDoc
		if err := r.store.col(colWorkspaces).FindOne(ctx, bson.D{{Key: "installKey", Value: installKey}}).Decode(&ws); err != nil {
			return err
		}
		res.WorkspaceId = ws.Id.Hex()
		res.WorkspaceCreated = up.UpsertedCount == 1

		// Settings version 1.
		up, err = r.store.col(colSettings).UpdateOne(ctx,
			bson.D{{Key: "workspaceId", Value: ws.Id}, {Key: "scope", Value: "installation"}},
			bson.D{{Key: "$setOnInsert", Value: bson.D{
				{Key: "_id", Value: bson.NewObjectID()}, {Key: "configVersion", Value: int64(1)},
				{Key: "data", Value: toSettingsData(seed.Settings)}, {Key: "updatedAt", Value: now},
				{Key: "updatedBy", Value: actorDoc{Type: seed.Actor.Type, Id: seed.Actor.Id, Label: seed.Actor.Label}},
			}}},
			options.UpdateOne().SetUpsert(true))
		if err != nil {
			return err
		}
		res.SettingsCreated = up.UpsertedCount == 1

		// Built-in statuses (installation-wide, project_id null).
		for _, s := range seed.Statuses {
			up, err := r.store.col(colStatuses).UpdateOne(ctx,
				bson.D{{Key: "workspaceId", Value: ws.Id}, {Key: "projectId", Value: nil}, {Key: "code", Value: s.Code}},
				bson.D{{Key: "$setOnInsert", Value: bson.D{
					{Key: "_id", Value: bson.NewObjectID()}, {Key: "label", Value: s.Label},
					{Key: "category", Value: string(s.Category)}, {Key: "builtin", Value: s.Builtin},
					{Key: "active", Value: true}, {Key: "order", Value: int64(s.Order)}, {Key: "createdAt", Value: now},
				}}},
				options.UpdateOne().SetUpsert(true))
			if err != nil {
				return err
			}
			res.StatusesCreated += int(up.UpsertedCount)
		}

		// Default request types.
		for _, t := range seed.RequestTypes {
			up, err := r.store.col(colRequestTypes).UpdateOne(ctx,
				bson.D{{Key: "workspaceId", Value: ws.Id}, {Key: "projectId", Value: nil}, {Key: "code", Value: t.Code}},
				bson.D{{Key: "$setOnInsert", Value: bson.D{
					{Key: "_id", Value: bson.NewObjectID()}, {Key: "name", Value: t.Name},
					{Key: "active", Value: true}, {Key: "order", Value: int64(t.Order)}, {Key: "createdAt", Value: now},
				}}},
				options.UpdateOne().SetUpsert(true))
			if err != nil {
				return err
			}
			res.RequestTypesCreated += int(up.UpsertedCount)
		}

		var events []any
		if res.WorkspaceCreated {
			events = append(events, toAuditDoc(ws.Id, audit.Event{Action: audit.InstallationInitialized, Actor: seed.Actor,
				Target: "workspace/" + ws.Id.Hex(), CreatedAt: now}))
		}

		// The deployment's single project and its protected Triage group.
		var existing projectDoc
		err = r.store.col(colProjects).FindOne(ctx, bson.D{{Key: "workspaceId", Value: ws.Id}},
			options.FindOne().SetSort(bson.D{{Key: "_id", Value: 1}})).Decode(&existing)
		switch {
		case err == nil:
		case errors.Is(err, mongo.ErrNoDocuments):
			pid, gid := bson.NewObjectID(), bson.NewObjectID()
			if _, err := r.store.col(colProjects).InsertOne(ctx, bson.D{
				{Key: "_id", Value: pid}, {Key: "workspaceId", Value: ws.Id}, {Key: "key", Value: seed.Project.Key},
				{Key: "name", Value: seed.Project.Name}, {Key: "status", Value: "active"},
				{Key: "defaultGroupId", Value: gid}, {Key: "createdAt", Value: now}, {Key: "updatedAt", Value: now},
				{Key: "version", Value: int64(1)},
			}); err != nil {
				return err
			}
			if _, err := r.store.col(colGroups).InsertOne(ctx, groupDoc{Id: gid, WorkspaceId: ws.Id, ProjectId: pid,
				Name: projects.TriageGroupName, NameNormalized: projects.NormalizeName(projects.TriageGroupName),
				Description: "Default queue for new tickets.", Active: true, IsDefault: true, CreatedAt: now, UpdatedAt: now, Version: 1}); err != nil {
				return err
			}
			// Starting hold reasons (only with the project, so an admin's deletion
			// is not undone by running init again).
			for _, hr := range seed.HoldReasons {
				if _, err := r.store.col(colHoldReasons).InsertOne(ctx, newHoldReasonDoc(holdreasons.Reason{Id: bson.NewObjectID().Hex(),
					WorkspaceId: ws.Id.Hex(), ProjectId: pid.Hex(), Name: hr.Name, MaxDays: hr.MaxDays, CreatedAt: now, Version: 1})); err != nil {
					return err
				}
			}
			res.ProjectCreated = true
			events = append(events,
				toAuditDoc(ws.Id, audit.Event{Action: audit.ProjectCreated, Actor: seed.Actor, Target: "project/" + pid.Hex(),
					ProjectId: pid.Hex(), Changes: []string{"key", "name"}, CreatedAt: now}),
				toAuditDoc(ws.Id, audit.Event{Action: audit.GroupCreated, Actor: seed.Actor, Target: "group/" + gid.Hex(),
					ProjectId: pid.Hex(), Changes: []string{"name"}, Reason: "default Triage group", CreatedAt: now}))
		default:
			return err
		}

		// First administrator: claimed through the workspace document so only
		// one bootstrap administrator can ever be created.
		if seed.Admin != nil && ws.BootstrapAdminId == nil {
			adminID := bson.NewObjectID()
			claim, err := r.store.col(colWorkspaces).UpdateOne(ctx,
				bson.D{{Key: "_id", Value: ws.Id}, {Key: "bootstrapAdminId", Value: nil}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "bootstrapAdminId", Value: adminID}}}})
			if err != nil {
				return err
			}
			if claim.ModifiedCount == 1 {
				_, err = r.store.col(colUsers).InsertOne(ctx, bson.D{
					{Key: "_id", Value: adminID}, {Key: "workspaceId", Value: ws.Id},
					{Key: "email", Value: seed.Admin.Email}, {Key: "emailNormalized", Value: seed.Admin.EmailNormalized},
					{Key: "username", Value: seed.Admin.Username},
					{Key: "displayName", Value: seed.Admin.Name.Full()}, {Key: "givenName", Value: seed.Admin.Name.Given},
					{Key: "middleName", Value: seed.Admin.Name.Middle}, {Key: "familyName", Value: seed.Admin.Name.Family},
					{Key: "passwordHash", Value: seed.Admin.PasswordHash},
					{Key: "status", Value: users.StatusActive}, {Key: "roles", Value: bson.A{users.RoleAdmin}},
					// The operator supplied this address locally; invitation-based
					// verification applies to accounts created later.
					{Key: "emailVerifiedAt", Value: now},
					{Key: "createdAt", Value: now}, {Key: "updatedAt", Value: now}, {Key: "version", Value: int64(1)},
				})
				if err != nil {
					return err
				}
				res.AdminCreated = true
				events = append(events, toAuditDoc(ws.Id, audit.Event{Action: audit.UserCreated, Actor: seed.Actor,
					Target: "user/" + adminID.Hex(), Changes: []string{"roles"}, Reason: "bootstrap administrator", CreatedAt: now}))
			}
		}
		if len(events) > 0 {
			if _, err := r.store.col(colAuditEvents).InsertMany(ctx, events); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return installation.Result{}, err
	}
	return res, nil
}
