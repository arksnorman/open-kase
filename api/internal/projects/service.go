package projects

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Service implements project, group and group-membership administration.
type Service struct {
	repo  Repository
	clock clock.Clock
}

// NewService returns a projects service.
func NewService(repo Repository, clk clock.Clock) *Service { return &Service{repo: repo, clock: clk} }

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrGroupNameTaken):
		return fault.Conflict("groupNameTaken", "A group with this name already exists in the project.")
	case errors.Is(err, ErrVersionConflict):
		return fault.Conflict("versionConflict", "This record was changed by someone else. Reload and try again.")
	case errors.Is(err, ErrUserNotFound):
		return fault.Field("userId", "notFound", "No user with this ID exists.")
	case errors.Is(err, ErrGroupProtected):
		return fault.Conflict("groupProtected", "The Triage group is protected and cannot be deactivated or removed.")
	case errors.Is(err, ErrGroupInUse):
		return fault.Conflict("groupInUse", "The group is referenced by tickets; deactivate it instead.")
	case errors.Is(err, ErrGroupInactive):
		return fault.Conflict("groupInactive", "The group is inactive.")
	}
	return err
}

// ---- Project ----

// GetProject returns the scope's project.
func (s *Service) GetProject(ctx context.Context, scope *access.ProjectScope) (Project, error) {
	if err := scope.Require(access.CapProjectRead); err != nil {
		return Project{}, err
	}
	pr, err := s.repo.GetProject(ctx, scope.Principal.WorkspaceId, scope.Project.Id)
	if errors.Is(err, ErrNotFound) {
		return Project{}, fault.NotFound("projectNotFound")
	}
	return pr, err
}

// UpdateProjectInput is a PATCH. The product name and description live in
// the settings (Configuration → General); tickets have no key prefix.
type UpdateProjectInput struct {
	// RequesterAcknowledgement toggles the requester's "ticket received" email.
	RequesterAcknowledgement *bool
}

// UpdateProject edits the requester acknowledgement setting (admins).
func (s *Service) UpdateProject(ctx context.Context, scope *access.ProjectScope, expectedVersion int64, in UpdateProjectInput) (Project, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Project{}, err
	}
	pr, err := s.GetProject(ctx, scope)
	if err != nil {
		return Project{}, err
	}
	if pr.Version != expectedVersion {
		return Project{}, mapErr(ErrVersionConflict)
	}
	var changes []string
	if in.RequesterAcknowledgement != nil && *in.RequesterAcknowledgement != pr.RequesterAcknowledgement {
		pr.RequesterAcknowledgement, changes = *in.RequesterAcknowledgement, append(changes, "requesterAcknowledgement")
	}
	if len(changes) == 0 {
		return pr, nil
	}
	now := s.clock.Now()
	pr.UpdatedAt = now
	ev := audit.New(ctx, scope.AuditActor(), audit.ProjectUpdated, "project/"+pr.Id, now)
	ev.ProjectId, ev.Changes = pr.Id, changes
	out, err := s.repo.UpdateProject(ctx, pr, expectedVersion, ev)
	return out, mapErr(err)
}

// ---- Groups ----

// CreateGroupInput is the create-group command.
type CreateGroupInput struct {
	Name        string
	Description string
}

// CreateGroup adds a group to the project.
func (s *Service) CreateGroup(ctx context.Context, scope *access.ProjectScope, in CreateGroupInput) (Group, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Group{}, err
	}
	var v validation.Collector
	v.String(validation.GroupName, "name", in.Name)
	v.String(validation.GroupDescription, "description", in.Description)
	if err := v.Err(); err != nil {
		return Group{}, err
	}
	now := s.clock.Now()
	g := Group{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, Name: in.Name, Description: in.Description,
		Active: true, CreatedAt: now, UpdatedAt: now, Version: 1}
	ev := audit.New(ctx, scope.AuditActor(), audit.GroupCreated, "", now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name", "description"}
	out, err := s.repo.CreateGroup(ctx, g, ev)
	return out, mapErr(err)
}

// visibleGroups returns nil (all groups) or the group IDs the scope may see.
// Every staff member sees every group: agents transfer tickets to groups they
// are not in, and a group's name grants no access to its tickets.
func visibleGroups(scope *access.ProjectScope) []string {
	if scope.Principal.IsStaff() || scope.Can(access.CapGroupReadAll) {
		return nil
	}
	if scope.GroupIds == nil {
		return []string{}
	}
	return scope.GroupIds
}

// ListGroups pages through the groups visible in the scope (every group, for
// staff).
func (s *Service) ListGroups(ctx context.Context, scope *access.ProjectScope, after *pagination.Cursor, limit int) (pagination.Page[Group], error) {
	if err := scope.Principal.RequireStaff(); err != nil {
		return pagination.Page[Group]{}, err
	}
	// member.read: users, and agent tokens with tickets:read.
	if err := scope.Require(access.CapMemberRead); err != nil {
		return pagination.Page[Group]{}, err
	}
	rows, err := s.repo.ListGroups(ctx, scope.Project.Id, visibleGroups(scope), after, limit+1)
	if err != nil {
		return pagination.Page[Group]{}, err
	}
	return pagination.NewPage(rows, limit, func(g Group) pagination.Cursor {
		return pagination.Cursor{Sort: NormalizeName(g.Name), Id: g.Id}
	}), nil
}

// GetGroup returns a visible group or 404.
func (s *Service) GetGroup(ctx context.Context, scope *access.ProjectScope, id string) (Group, error) {
	if err := scope.Principal.RequireStaff(); err != nil {
		return Group{}, err
	}
	if err := scope.Require(access.CapMemberRead); err != nil {
		return Group{}, err
	}
	notFound := fault.NotFound("groupNotFound")
	if !validation.ValidID(id) {
		return Group{}, notFound
	}
	if only := visibleGroups(scope); only != nil && !slices.Contains(only, id) {
		return Group{}, notFound
	}
	g, err := s.repo.GetGroup(ctx, scope.Project.Id, id)
	if errors.Is(err, ErrNotFound) {
		return Group{}, notFound
	}
	return g, err
}

// UpdateGroupInput is a group PATCH.
type UpdateGroupInput struct {
	Name        *string
	Description *string
	Active      *bool
}

// UpdateGroup renames, describes or (de)activates a group. The protected
// Triage group cannot be deactivated, but its label may be renamed: its
// protected identity is the stored default-group flag (and the project's
// defaultGroupId), never the name.
func (s *Service) UpdateGroup(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64, in UpdateGroupInput) (Group, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Group{}, err
	}
	var v validation.Collector
	v.OptionalString(validation.GroupName, "name", in.Name)
	v.OptionalString(validation.GroupDescription, "description", in.Description)
	if err := v.Err(); err != nil {
		return Group{}, err
	}
	g, err := s.GetGroup(ctx, scope, id)
	if err != nil {
		return Group{}, err
	}
	if g.Version != expectedVersion {
		return Group{}, mapErr(ErrVersionConflict)
	}
	var changes []string
	if in.Name != nil && *in.Name != g.Name {
		g.Name, changes = *in.Name, append(changes, "name")
	}
	if in.Description != nil && *in.Description != g.Description {
		g.Description, changes = *in.Description, append(changes, "description")
	}
	action := audit.GroupUpdated
	if in.Active != nil && *in.Active != g.Active {
		if g.Protected && !*in.Active {
			return Group{}, mapErr(ErrGroupProtected)
		}
		g.Active, changes = *in.Active, append(changes, "active")
		action = map[bool]audit.Action{true: audit.GroupActivated, false: audit.GroupDeactivated}[g.Active]
	}
	if len(changes) == 0 {
		return g, nil
	}
	now := s.clock.Now()
	g.UpdatedAt = now
	ev := audit.New(ctx, scope.AuditActor(), action, "group/"+g.Id, now)
	ev.ProjectId, ev.Changes = g.ProjectId, changes
	out, err := s.repo.UpdateGroup(ctx, g, expectedVersion, ev)
	return out, mapErr(err)
}

// DeleteGroup removes an unreferenced, unprotected group and its group
// memberships. Groups referenced by tickets must be deactivated instead.
func (s *Service) DeleteGroup(ctx context.Context, scope *access.ProjectScope, id string) error {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return err
	}
	g, err := s.GetGroup(ctx, scope, id)
	if err != nil {
		return err
	}
	if g.Protected {
		return mapErr(ErrGroupProtected)
	}
	now := s.clock.Now()
	ev := audit.New(ctx, scope.AuditActor(), audit.GroupDeleted, "group/"+g.Id, now)
	ev.ProjectId = g.ProjectId
	return mapErr(s.repo.DeleteGroup(ctx, g.ProjectId, g.Id, ev))
}

// ---- Member directory ----

// ListMembers pages through every user with their role and groups (the
// assignee and watcher pickers read it).
func (s *Service) ListMembers(ctx context.Context, scope *access.ProjectScope, f MemberFilter, after *pagination.Cursor, limit int) (pagination.Page[Member], error) {
	if err := scope.Principal.RequireStaff(); err != nil {
		return pagination.Page[Member]{}, err
	}
	if err := scope.Require(access.CapMemberRead); err != nil {
		return pagination.Page[Member]{}, err
	}
	var v validation.Collector
	v.String(validation.SearchQuery, "q", f.Q)
	if f.GroupId != "" {
		v.String(validation.ObjectID, "groupId", f.GroupId)
	}
	if len(f.UserIds) > MaxMemberUserIds {
		v.Add("userId", validation.CodeTooLong, "Use at most 100 user IDs.")
	}
	for _, id := range f.UserIds {
		v.String(validation.ObjectID, "userId", id)
	}
	if err := v.Err(); err != nil {
		return pagination.Page[Member]{}, err
	}
	if f.GroupId != "" {
		g, err := s.GetGroup(ctx, scope, f.GroupId)
		if err != nil {
			return pagination.Page[Member]{}, err
		}
		if !g.Active {
			return pagination.Page[Member]{}, fault.Field("groupId", "groupInactive", "Only active groups can be listed.")
		}
	}
	f.Q = strings.TrimSpace(f.Q)
	rows, err := s.repo.ListMembers(ctx, scope.Principal.WorkspaceId, scope.Project.Id, f, after, limit+1)
	if err != nil {
		return pagination.Page[Member]{}, err
	}
	return pagination.NewPage(rows, limit, func(m Member) pagination.Cursor { return pagination.Cursor{Id: m.UserId} }), nil
}

// ---- Group memberships ----

// AddGroupMember adds a user to a group (idempotent).
func (s *Service) AddGroupMember(ctx context.Context, scope *access.ProjectScope, groupID, userID string) (GroupMember, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return GroupMember{}, err
	}
	g, err := s.GetGroup(ctx, scope, groupID)
	if err != nil {
		return GroupMember{}, err
	}
	if !validation.ValidID(userID) {
		return GroupMember{}, fault.NotFound("memberNotFound")
	}
	now := s.clock.Now()
	ev := audit.New(ctx, scope.AuditActor(), audit.GroupMembershipAdded, "group/"+g.Id+"/user/"+userID, now)
	ev.ProjectId = g.ProjectId
	gm, err := s.repo.AddGroupMember(ctx, scope.Principal.WorkspaceId, g.ProjectId, g.Id, userID, now, ev)
	if errors.Is(err, ErrNotFound) {
		return GroupMember{}, fault.NotFound("groupNotFound")
	}
	return gm, mapErr(err)
}

// RemoveGroupMember removes a user from a group (idempotent).
func (s *Service) RemoveGroupMember(ctx context.Context, scope *access.ProjectScope, groupID, userID string) error {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return err
	}
	g, err := s.GetGroup(ctx, scope, groupID)
	if err != nil {
		return err
	}
	if !validation.ValidID(userID) {
		return nil
	}
	now := s.clock.Now()
	ev := audit.New(ctx, scope.AuditActor(), audit.GroupMembershipRemoved, "group/"+g.Id+"/user/"+userID, now)
	ev.ProjectId = g.ProjectId
	return mapErr(s.repo.RemoveGroupMember(ctx, g.ProjectId, g.Id, userID, ev))
}

// ListGroupMembers pages through a visible group's members.
func (s *Service) ListGroupMembers(ctx context.Context, scope *access.ProjectScope, groupID string, after *pagination.Cursor, limit int) (pagination.Page[GroupMember], error) {
	g, err := s.GetGroup(ctx, scope, groupID)
	if err != nil {
		return pagination.Page[GroupMember]{}, err
	}
	rows, err := s.repo.ListGroupMembers(ctx, g.ProjectId, g.Id, after, limit+1)
	if err != nil {
		return pagination.Page[GroupMember]{}, err
	}
	return pagination.NewPage(rows, limit, func(m GroupMember) pagination.Cursor {
		return pagination.Cursor{Id: m.UserId}
	}), nil
}

// ---- A user's groups (the admin user page) ----

// maxUserGroups bounds the group IDs in one SetUserGroups call.
const maxUserGroups = 200

// UserGroups returns the active groups a user belongs to (administrators).
// The caller checks that the user exists.
func (s *Service) UserGroups(ctx context.Context, scope *access.ProjectScope, userID string) ([]GroupRef, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return nil, err
	}
	return s.repo.UserGroupRefs(ctx, scope.Project.Id, userID)
}

// SetUserGroups makes a user's active-group memberships equal groupIDs:
// missing memberships are added and the others removed through
// AddGroupMember and RemoveGroupMember, so each change is audited and a
// removal releases the user's ineligible assignments exactly as on the
// group's member routes. Every group is checked first (visible and active)
// so invalid input changes nothing; a failure part-way (e.g. a group
// deactivated concurrently) leaves the earlier changes applied, and
// repeating the call converges. The caller checks that the user exists.
func (s *Service) SetUserGroups(ctx context.Context, scope *access.ProjectScope, userID string, groupIDs []string) ([]GroupRef, error) {
	if err := scope.Require(access.CapProjectManage); err != nil {
		return nil, err
	}
	if len(groupIDs) > maxUserGroups {
		return nil, fault.Field("groupIds", validation.CodeTooLong, "Too many groups.")
	}
	want := map[string]bool{}
	for _, id := range groupIDs {
		if want[id] {
			continue
		}
		g, err := s.GetGroup(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		if !g.Active {
			return nil, mapErr(ErrGroupInactive)
		}
		want[id] = true
	}
	current, err := s.repo.UserGroupRefs(ctx, scope.Project.Id, userID)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, g := range current {
		have[g.Id] = true
		if !want[g.Id] {
			if err := s.RemoveGroupMember(ctx, scope, g.Id, userID); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range groupIDs {
		if want[id] && !have[id] {
			have[id] = true
			if _, err := s.AddGroupMember(ctx, scope, id, userID); err != nil {
				return nil, err
			}
		}
	}
	return s.repo.UserGroupRefs(ctx, scope.Project.Id, userID)
}
