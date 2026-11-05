// Package projects manages the deployment's single project (created by
// init; an internal record holding a few settings), its groups (queues,
// including the protected Triage group), group memberships and the
// read-only member directory (every user with their role and groups).
package projects

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

// TriageGroupName is the protected default group every project receives.
const TriageGroupName = "Triage"

// Project is the deployment's support boundary. Every stored record carries
// its ID.
type Project struct {
	Id             string
	WorkspaceId    string
	DefaultGroupId string
	// RequesterAcknowledgement sends the requester contact an email when a
	// ticket is created for them (default on).
	RequesterAcknowledgement bool
	CreatedAt                time.Time
	UpdatedAt                time.Time
	Version                  int64
}

// Group is a queue (team).
type Group struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	Name        string
	Description string
	Active      bool
	// Protected marks the default Triage group: it cannot be deactivated or
	// deleted.
	Protected   bool
	MemberCount int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Version     int64
}

// GroupRef is a compact group reference.
type GroupRef struct {
	Id   string
	Name string
}

// Member is a directory entry: a user with their role (admin or agent) and
// the active groups they belong to.
type Member struct {
	UserId    string
	Role      string
	UserName  string
	UserEmail string
	// UserUsername is the member's sign-in username.
	UserUsername string
	UserStatus   string
	Groups       []GroupRef
	CreatedAt    time.Time
}

// MaxMemberUserIds bounds MemberFilter.UserIds.
const MaxMemberUserIds = 100

// MemberFilter narrows the member directory (every field optional).
type MemberFilter struct {
	// Q is a case-insensitive substring of the name, email or username.
	Q string
	// GroupId lists only members of this active group.
	GroupId string
	// UserIds lists only these users (at most MaxMemberUserIds).
	UserIds []string
	// ActiveOnly drops disabled and invited users.
	ActiveOnly bool
}

// GroupMember is a user's membership in a group.
type GroupMember struct {
	GroupId   string
	UserId    string
	Name      string
	Email     string
	Username  string
	CreatedAt time.Time
}

// Repository errors.
var (
	ErrNotFound        = errors.New("projects: not found")
	ErrGroupNameTaken  = errors.New("projects: group name already in use")
	ErrVersionConflict = errors.New("projects: version conflict")
	ErrUserNotFound    = errors.New("projects: user not found")
	ErrGroupProtected  = errors.New("projects: group is protected")
	ErrGroupInUse      = errors.New("projects: group is referenced")
	ErrGroupInactive   = errors.New("projects: group is inactive")
)

// Repository persists the project and its groups. Every mutation writes its audit
// event(s) in the same transaction.
type Repository interface {
	GetProject(ctx context.Context, workspaceID, id string) (Project, error)
	UpdateProject(ctx context.Context, p Project, expectedVersion int64, ev audit.Event) (Project, error)

	CreateGroup(ctx context.Context, g Group, ev audit.Event) (Group, error)
	ListGroups(ctx context.Context, projectID string, only []string, after *pagination.Cursor, limit int) ([]Group, error)
	GetGroup(ctx context.Context, projectID, id string) (Group, error)
	UpdateGroup(ctx context.Context, g Group, expectedVersion int64, ev audit.Event) (Group, error)
	DeleteGroup(ctx context.Context, projectID, id string, ev audit.Event) error

	// ListMembers pages through the workspace's users in creation order.
	ListMembers(ctx context.Context, workspaceID, projectID string, f MemberFilter, after *pagination.Cursor, limit int) ([]Member, error)

	AddGroupMember(ctx context.Context, workspaceID, projectID, groupID, userID string, now time.Time, ev audit.Event) (GroupMember, error)
	RemoveGroupMember(ctx context.Context, projectID, groupID, userID string, ev audit.Event) error
	ListGroupMembers(ctx context.Context, projectID, groupID string, after *pagination.Cursor, limit int) ([]GroupMember, error)
	// UserGroupRefs lists the active groups a user belongs to, by name.
	UserGroupRefs(ctx context.Context, projectID, userID string) ([]GroupRef, error)
}

// NormalizeName returns the case-insensitive uniqueness form of a name.
func NormalizeName(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
