package mongo

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

type projectDoc struct {
	Id             bson.ObjectID  `bson:"_id"`
	WorkspaceId    bson.ObjectID  `bson:"workspaceId"`
	DefaultGroupId *bson.ObjectID `bson:"defaultGroupId"`
	// RequesterAckDisabled is stored inverted so existing projects default
	// to sending acknowledgements.
	RequesterAckDisabled bool      `bson:"requesterAckDisabled,omitempty"`
	CreatedAt            time.Time `bson:"createdAt"`
	UpdatedAt            time.Time `bson:"updatedAt"`
	Version              int64     `bson:"version"`
}

func (d projectDoc) toDomain() projects.Project {
	return projects.Project{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(),
		DefaultGroupId: hexOrEmpty(d.DefaultGroupId), RequesterAcknowledgement: !d.RequesterAckDisabled,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Version: d.Version}
}

type groupDoc struct {
	Id             bson.ObjectID `bson:"_id"`
	WorkspaceId    bson.ObjectID `bson:"workspaceId"`
	ProjectId      bson.ObjectID `bson:"projectId"`
	Name           string        `bson:"name"`
	NameNormalized string        `bson:"nameNormalized"`
	Description    string        `bson:"description"`
	Active         bool          `bson:"active"`
	IsDefault      bool          `bson:"isDefault"`
	CreatedAt      time.Time     `bson:"createdAt"`
	UpdatedAt      time.Time     `bson:"updatedAt"`
	Version        int64         `bson:"version"`
}

func (d groupDoc) toDomain() projects.Group {
	return projects.Group{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Name: d.Name,
		Description: d.Description, Active: d.Active, Protected: d.IsDefault, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Version: d.Version}
}

type groupMembershipDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	ProjectId   bson.ObjectID `bson:"projectId"`
	GroupId     bson.ObjectID `bson:"groupId"`
	UserId      bson.ObjectID `bson:"userId"`
	CreatedAt   time.Time     `bson:"createdAt"`
}

// ProjectsRepository implements projects.Repository and access.Directory.
type ProjectsRepository struct{ store *Store }

// NewProjectsRepository returns a projects repository.
func NewProjectsRepository(s *Store) *ProjectsRepository { return &ProjectsRepository{store: s} }

// ---- access.Directory ----

// TheProject returns the workspace's project: the one init created. A
// database from before the single-project change may hold several; the
// oldest is the deployment's project.
func (r *ProjectsRepository) TheProject(ctx context.Context, workspaceID string) (access.ProjectRecord, error) {
	ws, err := oid(workspaceID)
	if err != nil {
		return access.ProjectRecord{}, access.ErrNotFound
	}
	var doc projectDoc
	err = r.store.col(colProjects).FindOne(ctx, bson.D{{Key: "workspaceId", Value: ws}},
		options.FindOne().SetSort(bson.D{{Key: "_id", Value: 1}})).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return access.ProjectRecord{}, access.ErrNotFound
	}
	if err != nil {
		return access.ProjectRecord{}, err
	}
	return access.ProjectRecord{Id: doc.Id.Hex(), WorkspaceId: doc.WorkspaceId.Hex()}, nil
}

// UserGroups returns the IDs of the active groups a user belongs to.
func (r *ProjectsRepository) UserGroups(ctx context.Context, projectID, userID string) ([]string, error) {
	pid, err1 := oid(projectID)
	uid, err2 := oid(userID)
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	groups, err := r.userGroups(ctx, pid, uid)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.Id)
	}
	return ids, nil
}

// userGroups returns the active groups a user belongs to in a project.
func (r *ProjectsRepository) userGroups(ctx context.Context, projectID, userID bson.ObjectID) ([]projects.GroupRef, error) {
	gms, err := findAll[groupMembershipDoc](ctx, r.store.col(colGroupMemberships),
		bson.D{{Key: "projectId", Value: projectID}, {Key: "userId", Value: userID}}, options.Find())
	if err != nil || len(gms) == 0 {
		return []projects.GroupRef{}, err
	}
	ids := make(bson.A, 0, len(gms))
	for _, gm := range gms {
		ids = append(ids, gm.GroupId)
	}
	groups, err := findAll[groupDoc](ctx, r.store.col(colGroups),
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "projectId", Value: projectID}, {Key: "active", Value: true}},
		options.Find().SetSort(bson.D{{Key: "nameNormalized", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]projects.GroupRef, 0, len(groups))
	for _, g := range groups {
		out = append(out, projects.GroupRef{Id: g.Id.Hex(), Name: g.Name})
	}
	return out, nil
}

// UserGroupRefs lists the active groups a user belongs to, ordered by name.
func (r *ProjectsRepository) UserGroupRefs(ctx context.Context, projectID, userID string) ([]projects.GroupRef, error) {
	uid, err := oid(userID)
	if err != nil {
		return []projects.GroupRef{}, nil
	}
	return r.userGroups(ctx, mustOID(projectID), uid)
}

// ---- Project ----

// GetProject returns a project by ID.
func (r *ProjectsRepository) GetProject(ctx context.Context, workspaceID, id string) (projects.Project, error) {
	pid, err := oid(id)
	if err != nil {
		return projects.Project{}, projects.ErrNotFound
	}
	var doc projectDoc
	err = r.store.col(colProjects).FindOne(ctx, bson.D{{Key: "_id", Value: pid}, {Key: "workspaceId", Value: mustOID(workspaceID)}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return projects.Project{}, projects.ErrNotFound
	}
	if err != nil {
		return projects.Project{}, err
	}
	return doc.toDomain(), nil
}

// versionedUpdate applies update to the document matching filter+version
// and decodes the new state into out; it distinguishes a version conflict
// from a missing document.
func (r *ProjectsRepository) versionedUpdate(ctx context.Context, col string, filter bson.D, expected int64, set bson.D, out any, notFound, conflict error) error {
	f := append(slices.Clone(filter), bson.E{Key: "version", Value: expected})
	err := r.store.col(col).FindOneAndUpdate(ctx, f, bson.D{
		{Key: "$set", Value: set}, {Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}},
	}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		if n, _ := r.store.col(col).CountDocuments(ctx, filter); n > 0 {
			return conflict
		}
		return notFound
	}
	return err
}

// UpdateProject writes the editable project fields with a version check.
func (r *ProjectsRepository) UpdateProject(ctx context.Context, p projects.Project, expectedVersion int64, ev audit.Event) (projects.Project, error) {
	ws := mustOID(p.WorkspaceId)
	var doc projectDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if err := r.versionedUpdate(ctx, colProjects, bson.D{{Key: "_id", Value: mustOID(p.Id)}, {Key: "workspaceId", Value: ws}}, expectedVersion,
			bson.D{{Key: "requesterAckDisabled", Value: !p.RequesterAcknowledgement}, {Key: "updatedAt", Value: p.UpdatedAt}},
			&doc, projects.ErrNotFound, projects.ErrVersionConflict); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		return projects.Project{}, err
	}
	return doc.toDomain(), nil
}

// ---- Groups ----

// CreateGroup inserts a group with its audit event.
func (r *ProjectsRepository) CreateGroup(ctx context.Context, g projects.Group, ev audit.Event) (projects.Group, error) {
	doc := groupDoc{Id: bson.NewObjectID(), WorkspaceId: mustOID(g.WorkspaceId), ProjectId: mustOID(g.ProjectId), Name: g.Name,
		NameNormalized: projects.NormalizeName(g.Name), Description: g.Description, Active: g.Active, IsDefault: false,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, Version: 1}
	ev.Target = "group/" + doc.Id.Hex()
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colGroups).InsertOne(ctx, doc); err != nil {
			if isDuplicate(err, "projectNameUnique") {
				return projects.ErrGroupNameTaken
			}
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return projects.Group{}, err
	}
	return doc.toDomain(), nil
}

func (r *ProjectsRepository) memberCounts(ctx context.Context, groupIDs []bson.ObjectID) (map[bson.ObjectID]int, error) {
	out := map[bson.ObjectID]int{}
	if len(groupIDs) == 0 {
		return out, nil
	}
	cur, err := r.store.col(colGroupMemberships).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "groupId", Value: bson.D{{Key: "$in", Value: groupIDs}}}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$groupId"}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
	})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Id bson.ObjectID `bson:"_id"`
		N  int           `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.Id] = row.N
	}
	return out, nil
}

// ListGroups pages through a project's groups ordered by name. A non-nil
// only restricts the result to those group IDs.
func (r *ProjectsRepository) ListGroups(ctx context.Context, projectID string, only []string, after *pagination.Cursor, limit int) ([]projects.Group, error) {
	filter := bson.D{{Key: "projectId", Value: mustOID(projectID)}}
	if only != nil {
		ids := bson.A{}
		for _, id := range only {
			ids = append(ids, mustOID(id))
		}
		filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}})
	}
	filter, err := pageFilter(filter, "nameNormalized", after)
	if err != nil {
		return nil, err
	}
	docs, err := findAll[groupDoc](ctx, r.store.col(colGroups), filter, pageOptions("nameNormalized", limit))
	if err != nil {
		return nil, err
	}
	ids := make([]bson.ObjectID, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.Id)
	}
	counts, err := r.memberCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]projects.Group, 0, len(docs))
	for _, d := range docs {
		g := d.toDomain()
		g.MemberCount = counts[d.Id]
		out = append(out, g)
	}
	return out, nil
}

// GetGroup returns a group in a project.
func (r *ProjectsRepository) GetGroup(ctx context.Context, projectID, id string) (projects.Group, error) {
	gid, err := oid(id)
	if err != nil {
		return projects.Group{}, projects.ErrNotFound
	}
	var doc groupDoc
	err = r.store.col(colGroups).FindOne(ctx, bson.D{{Key: "_id", Value: gid}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return projects.Group{}, projects.ErrNotFound
	}
	if err != nil {
		return projects.Group{}, err
	}
	g := doc.toDomain()
	counts, err := r.memberCounts(ctx, []bson.ObjectID{gid})
	if err != nil {
		return projects.Group{}, err
	}
	g.MemberCount = counts[gid]
	return g, nil
}

// UpdateGroup writes a group change with a version check. The protected
// default group can never be stored as inactive.
func (r *ProjectsRepository) UpdateGroup(ctx context.Context, g projects.Group, expectedVersion int64, ev audit.Event) (projects.Group, error) {
	filter := bson.D{{Key: "_id", Value: mustOID(g.Id)}, {Key: "projectId", Value: mustOID(g.ProjectId)}}
	var doc groupDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if !g.Active {
			var cur groupDoc
			if err := r.store.col(colGroups).FindOne(ctx, filter).Decode(&cur); err == nil && cur.IsDefault {
				return projects.ErrGroupProtected
			}
		}
		err := r.versionedUpdate(ctx, colGroups, filter, expectedVersion, bson.D{
			{Key: "name", Value: g.Name}, {Key: "nameNormalized", Value: projects.NormalizeName(g.Name)},
			{Key: "description", Value: g.Description}, {Key: "active", Value: g.Active}, {Key: "updatedAt", Value: g.UpdatedAt},
		}, &doc, projects.ErrNotFound, projects.ErrVersionConflict)
		if isDuplicate(err, "projectNameUnique") {
			return projects.ErrGroupNameTaken
		}
		if err != nil {
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return projects.Group{}, err
	}
	out := doc.toDomain()
	out.MemberCount = g.MemberCount
	return out, nil
}

// DeleteGroup removes an unprotected group that no ticket references, with
// its group memberships, in one transaction.
func (r *ProjectsRepository) DeleteGroup(ctx context.Context, projectID, id string, ev audit.Event) error {
	pid, gid := mustOID(projectID), mustOID(id)
	return r.store.transaction(ctx, func(ctx context.Context) error {
		var doc groupDoc
		err := r.store.col(colGroups).FindOne(ctx, bson.D{{Key: "_id", Value: gid}, {Key: "projectId", Value: pid}}).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return projects.ErrNotFound
		}
		if err != nil {
			return err
		}
		if doc.IsDefault {
			return projects.ErrGroupProtected
		}
		n, err := r.store.col(colTickets).CountDocuments(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "groupId", Value: gid}}, options.Count().SetLimit(1))
		if err != nil {
			return err
		}
		if n > 0 {
			return projects.ErrGroupInUse
		}
		if _, err := r.store.col(colGroups).DeleteOne(ctx, bson.D{{Key: "_id", Value: gid}, {Key: "isDefault", Value: false}}); err != nil {
			return err
		}
		if _, err := r.store.col(colGroupMemberships).DeleteMany(ctx, bson.D{{Key: "groupId", Value: gid}}); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
}

// ---- Member directory ----

// maxGroupMembers bounds the membership scan of a group-filtered member list.
const maxGroupMembers = 5000

// ListMembers pages through the workspace's users in creation order, each
// with their role and active groups.
func (r *ProjectsRepository) ListMembers(ctx context.Context, workspaceID, projectID string, f projects.MemberFilter, after *pagination.Cursor, limit int) ([]projects.Member, error) {
	base := bson.D{{Key: "workspaceId", Value: mustOID(workspaceID)}}
	if f.ActiveOnly {
		base = append(base, bson.E{Key: "status", Value: users.StatusActive})
	}
	if len(f.UserIds) > 0 {
		base = append(base, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: oidList(f.UserIds)}}})
	}
	if f.Q != "" {
		re := bson.Regex{Pattern: regexpQuote(f.Q), Options: "i"}
		base = append(base, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "displayName", Value: re}}, bson.D{{Key: "email", Value: re}}, bson.D{{Key: "username", Value: re}}}})
	}
	if f.GroupId != "" {
		ms, err := findAll[groupMembershipDoc](ctx, r.store.col(colGroupMemberships),
			bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "groupId", Value: mustOID(f.GroupId)}},
			options.Find().SetProjection(bson.D{{Key: "userId", Value: 1}}).SetLimit(maxGroupMembers))
		if err != nil {
			return nil, err
		}
		ids := make(bson.A, 0, len(ms))
		for _, m := range ms {
			ids = append(ids, m.UserId)
		}
		base = append(base, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}})
	}
	filter, err := pageFilter(base, "", after)
	if err != nil {
		return nil, err
	}
	docs, err := findAll[userDoc](ctx, r.store.col(colUsers), filter, pageOptions("", limit))
	if err != nil {
		return nil, err
	}
	pid := mustOID(projectID)
	out := make([]projects.Member, 0, len(docs))
	for _, u := range docs {
		groups, err := r.userGroups(ctx, pid, u.Id)
		if err != nil {
			return nil, err
		}
		role := access.RoleAgent
		if slices.Contains(u.Roles, users.RoleAdmin) {
			role = access.RoleAdmin
		}
		out = append(out, projects.Member{UserId: u.Id.Hex(), Role: role, UserName: u.DisplayName, UserEmail: u.Email,
			UserUsername: u.Username, UserStatus: u.Status, Groups: groups, CreatedAt: u.CreatedAt})
	}
	return out, nil
}

// ---- Group memberships ----

// AddGroupMember adds an existing user to an active group.
func (r *ProjectsRepository) AddGroupMember(ctx context.Context, workspaceID, projectID, groupID, userID string, now time.Time, ev audit.Event) (projects.GroupMember, error) {
	ws, pid, gid, uid := mustOID(workspaceID), mustOID(projectID), mustOID(groupID), mustOID(userID)
	var out projects.GroupMember
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		var g groupDoc
		err := r.store.col(colGroups).FindOne(ctx, bson.D{{Key: "_id", Value: gid}, {Key: "projectId", Value: pid}}).Decode(&g)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return projects.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !g.Active {
			return projects.ErrGroupInactive
		}
		n, err := r.store.col(colUsers).CountDocuments(ctx, bson.D{{Key: "_id", Value: uid}, {Key: "workspaceId", Value: ws}}, options.Count().SetLimit(1))
		if err != nil {
			return err
		}
		if n == 0 {
			return projects.ErrUserNotFound
		}
		doc := groupMembershipDoc{Id: bson.NewObjectID(), WorkspaceId: ws, ProjectId: pid, GroupId: gid, UserId: uid, CreatedAt: now}
		up, err := r.store.col(colGroupMemberships).UpdateOne(ctx, bson.D{{Key: "groupId", Value: gid}, {Key: "userId", Value: uid}},
			bson.D{{Key: "$setOnInsert", Value: doc}}, options.UpdateOne().SetUpsert(true))
		if err != nil {
			return err
		}
		var stored groupMembershipDoc
		if err := r.store.col(colGroupMemberships).FindOne(ctx, bson.D{{Key: "groupId", Value: gid}, {Key: "userId", Value: uid}}).Decode(&stored); err != nil {
			return err
		}
		out = projects.GroupMember{GroupId: groupID, UserId: userID, CreatedAt: stored.CreatedAt}
		if up.UpsertedCount == 1 {
			return r.store.insertAudit(ctx, ws, ev)
		}
		return nil
	})
	if err != nil {
		return projects.GroupMember{}, err
	}
	var u userDoc
	if err := r.store.col(colUsers).FindOne(ctx, bson.D{{Key: "_id", Value: uid}}).Decode(&u); err == nil {
		out.Name, out.Email, out.Username = u.DisplayName, u.Email, u.Username
	}
	return out, nil
}

// RemoveGroupMember deletes a group membership; removing a non-member is a
// no-op without an audit event.
func (r *ProjectsRepository) RemoveGroupMember(ctx context.Context, projectID, groupID, userID string, ev audit.Event) error {
	pid, gid, uid := mustOID(projectID), mustOID(groupID), mustOID(userID)
	return r.store.transaction(ctx, func(ctx context.Context) error {
		var doc groupMembershipDoc
		err := r.store.col(colGroupMemberships).FindOneAndDelete(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "groupId", Value: gid}, {Key: "userId", Value: uid}}).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.store.insertAccessEvent(ctx, doc.WorkspaceId, outbox.GroupMembershipRemoved, &pid, uid, &gid, &ev, ev.CreatedAt); err != nil {
			return err
		}
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
}

// ListGroupMembers pages through a group's members ordered by user ID.
func (r *ProjectsRepository) ListGroupMembers(ctx context.Context, projectID, groupID string, after *pagination.Cursor, limit int) ([]projects.GroupMember, error) {
	filter := bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "groupId", Value: mustOID(groupID)}}
	if after != nil {
		uid, err := oid(after.Id)
		if err != nil {
			return nil, errInvalidCursor
		}
		filter = append(filter, bson.E{Key: "userId", Value: bson.D{{Key: "$gt", Value: uid}}})
	}
	docs, err := findAll[groupMembershipDoc](ctx, r.store.col(colGroupMemberships), filter,
		options.Find().SetSort(bson.D{{Key: "userId", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	out := make([]projects.GroupMember, 0, len(docs))
	for _, d := range docs {
		gm := projects.GroupMember{GroupId: groupID, UserId: d.UserId.Hex(), CreatedAt: d.CreatedAt}
		var u userDoc
		if err := r.store.col(colUsers).FindOne(ctx, bson.D{{Key: "_id", Value: d.UserId}}).Decode(&u); err == nil {
			gm.Name, gm.Email, gm.Username = u.DisplayName, u.Email, u.Username
		}
		out = append(out, gm)
	}
	return out, nil
}
