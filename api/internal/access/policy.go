package access

import (
	"context"
	"errors"
	"slices"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/fault"
)

// ErrNotFound is returned by Directory lookups for missing records.
var ErrNotFound = errors.New("access: not found")

// ProjectRecord is the trusted data of the deployment's single project.
type ProjectRecord struct {
	Id          string
	WorkspaceId string
}

// Directory reads the trusted records that policy decisions depend on.
type Directory interface {
	// TheProject returns the workspace's project (created by init), or
	// ErrNotFound before initialization.
	TheProject(ctx context.Context, workspaceID string) (ProjectRecord, error)
	// UserGroups returns the active groups the user belongs to.
	UserGroups(ctx context.Context, projectID, userID string) ([]string, error)
}

// Service makes policy decisions.
type Service struct {
	dir Directory
}

// NewService returns a policy service.
func NewService(dir Directory) *Service { return &Service{dir: dir} }

// ProjectScope is the result of authorizing a principal. It is valid for the
// current request only. Project identifies the deployment's single project,
// whose ID every stored record carries.
type ProjectScope struct {
	Project   ProjectRecord
	Principal Principal
	// Role is the user's role (admin or agent); empty for integrations.
	Role     string
	GroupIds []string
	// caps maps each granted capability to the integration scope that
	// grants it (empty for user roles).
	caps map[Capability]string
	// used lists the integration scopes that allowed the request's checks
	// (Require), for the audit actor.
	used []string
}

// Can reports whether the scope grants c.
func (s *ProjectScope) Can(c Capability) bool {
	_, ok := s.caps[c]
	return ok
}

// Require returns a 403 error unless the scope grants c. For integration
// tokens it records the scope that allowed the check, so the audit events
// of the request name it (AuditActor).
func (s *ProjectScope) Require(c Capability) error {
	if granting, ok := s.caps[c]; ok {
		if granting != "" && !slices.Contains(s.used, granting) {
			s.used = append(slices.Clone(s.used), granting)
		}
		return nil
	}
	if s.Principal.Kind == KindIntegration {
		return fault.Forbidden("insufficientScope", "The integration credential does not have the scope required for this operation.")
	}
	return fault.Forbidden("forbidden", "Your role does not allow this operation.")
}

// AuditActor returns the audit actor of the request: the principal, and for
// an integration token the scopes that allowed the request. Read scopes
// (needed to address the ticket or contact first) are left out when a write
// scope was involved, so a write names the scope that allowed the change.
func (s *ProjectScope) AuditActor() audit.Actor {
	a := s.Principal.AuditActor()
	if s.Principal.Kind != KindIntegration || s.Principal.Actor != nil || len(s.used) == 0 {
		return a
	}
	isRead := func(sc string) bool { return sc == ScopeTicketsRead || sc == ScopeContactsRead }
	writes := slices.DeleteFunc(slices.Clone(s.used), isRead)
	if len(writes) > 0 {
		a.Scopes = writes
	} else {
		a.Scopes = slices.Clone(s.used)
	}
	return a
}

// InGroup reports whether the user is a member of groupID.
func (s *ProjectScope) InGroup(groupID string) bool { return slices.Contains(s.GroupIds, groupID) }

// errProjectNotFound is returned before init created the project, and to
// principals that cannot be scoped.
func errProjectNotFound() error { return fault.NotFound("projectNotFound") }

// Scope authorizes the principal: admins and agents by role (agents also by
// their groups), integrations by their scopes.
func (s *Service) Scope(ctx context.Context, p Principal) (*ProjectScope, error) {
	if p.WorkspaceId == "" {
		return nil, errProjectNotFound()
	}
	proj, err := s.dir.TheProject(ctx, p.WorkspaceId)
	if errors.Is(err, ErrNotFound) {
		return nil, errProjectNotFound()
	}
	if err != nil {
		return nil, err
	}
	scope := &ProjectScope{Project: proj, Principal: p}
	switch p.Kind {
	case KindIntegration:
		if p.ProjectId != proj.Id {
			return nil, errProjectNotFound()
		}
		scope.caps = ScopeCapabilities(p.Scopes)
		return scope, nil
	case KindUser:
		if scope.GroupIds, err = s.dir.UserGroups(ctx, proj.Id, p.Id); err != nil {
			return nil, err
		}
		scope.Role = RoleAgent
		if p.Admin {
			scope.Role = RoleAdmin
		}
		scope.caps = map[Capability]string{}
		for _, c := range RoleCapabilities(scope.Role) {
			scope.caps[c] = ""
		}
		return scope, nil
	}
	return nil, errProjectNotFound()
}
