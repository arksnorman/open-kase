package statuses

import (
	"context"

	"github.com/arksnorman/open-kase/api/internal/access"
)

// Status is a workflow status visible in a project: an installation-wide
// built-in or a project-defined custom status.
type Status struct {
	Id       string
	Code     string
	Name     string
	Category Category
	Builtin  bool
	Order    int
}

// RequestType classifies a ticket independently of its priority.
type RequestType struct {
	Id    string
	Code  string
	Name  string
	Order int
}

// CatalogRepository reads active workflow configuration for a project
// (installation-wide records plus the project's own).
type CatalogRepository interface {
	ProjectStatuses(ctx context.Context, workspaceID, projectID string) ([]Status, error)
	ProjectRequestTypes(ctx context.Context, workspaceID, projectID string) ([]RequestType, error)
}

// Catalog serves a project's workflow configuration.
type Catalog struct{ repo CatalogRepository }

// NewCatalog returns a workflow catalog service.
func NewCatalog(repo CatalogRepository) *Catalog { return &Catalog{repo: repo} }

// Statuses lists the statuses usable in the scope's project.
func (c *Catalog) Statuses(ctx context.Context, scope *access.ProjectScope) ([]Status, error) {
	if err := scope.Require(access.CapProjectRead); err != nil {
		return nil, err
	}
	return c.repo.ProjectStatuses(ctx, scope.Principal.WorkspaceId, scope.Project.Id)
}

// RequestTypes lists the request types usable in the scope's project.
func (c *Catalog) RequestTypes(ctx context.Context, scope *access.ProjectScope) ([]RequestType, error) {
	if err := scope.Require(access.CapProjectRead); err != nil {
		return nil, err
	}
	return c.repo.ProjectRequestTypes(ctx, scope.Principal.WorkspaceId, scope.Project.Id)
}
