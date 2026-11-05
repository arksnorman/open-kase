// Package integrations manages project-scoped service principals and their
// API credentials (API tokens). There is one kind of token; its creator
// picks the scopes (owner decision 2026-10-31): requester-side scopes let an
// external system create and follow tickets for the requester, staff scopes
// let an external AI agent or tool act as support staff under its own name
// (access.StaffScopes). Tokens are shown once and stored as SHA-256 hashes.
package integrations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/product"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// TokenPrefix marks open-kase integration tokens so that secret scanners
// and the authentication middleware can recognise them.
const TokenPrefix = product.TokenPrefix

// Status values.
const (
	StatusActive  = "active"
	StatusRevoked = "revoked"
)

// Integration is a service principal bound to one project.
type Integration struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	Name        string
	Scopes      []string
	Status      string
	TokenHint   string // first characters of the token, for identification
	CreatedBy   string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	Version     int64
}

// Credential is the authentication result for a token.
type Credential struct {
	Integration  Integration
	CredentialId string
}

// Repository errors.
var (
	ErrNotFound        = errors.New("integrations: not found")
	ErrVersionConflict = errors.New("integrations: version conflict")
	ErrRevoked         = errors.New("integrations: revoked")
)

// Repository persists integrations and credentials.
type Repository interface {
	// Create stores the principal and its credential hash with the audit event.
	Create(ctx context.Context, in Integration, tokenHash string, ev audit.Event) (Integration, error)
	List(ctx context.Context, workspaceID, projectID string, after *pagination.Cursor, limit int) ([]Integration, error)
	// Rename changes an active integration's name with a version check and
	// appends the audit event in the same transaction.
	Rename(ctx context.Context, workspaceID, projectID, id, name string, expectedVersion int64, ev audit.Event) (Integration, error)
	// Revoke revokes the principal and all its credentials (idempotent).
	Revoke(ctx context.Context, workspaceID, projectID, id string, now time.Time, ev audit.Event) error
	// FindByTokenHash returns an active credential and its principal.
	FindByTokenHash(ctx context.Context, hash string) (Credential, error)
	// TouchLastUsed records use at most once per interval.
	TouchLastUsed(ctx context.Context, c Credential, now time.Time, interval time.Duration) error
	// ProjectActive reports whether the project exists and is active.
	ProjectActive(ctx context.Context, workspaceID, projectID string) (bool, error)
}

// Service manages integrations and authenticates their tokens.
type Service struct {
	repo  Repository
	clock clock.Clock
}

// NewService returns an integrations service.
func NewService(repo Repository, clk clock.Clock) *Service { return &Service{repo: repo, clock: clk} }

// CreateInput is the create-integration command.
type CreateInput struct {
	Name   string
	Scopes []string
}

// Validate checks field rules: a name and at least one known scope.
func (in CreateInput) Validate() error {
	var v validation.Collector
	v.String(validation.IntegrationName, "name", in.Name)
	if len(in.Scopes) == 0 {
		v.Add("scopes", validation.CodeRequired, "Choose at least one scope.")
	}
	for i, sc := range in.Scopes {
		v.String(validation.IntegrationScope, "scopes["+strconv.Itoa(i)+"]", sc)
	}
	return v.Err()
}

// Created is the one-time create result; Token is never retrievable again.
type Created struct {
	Integration Integration
	Token       string
}

// Create issues a new integration credential for the scope's project.
func (s *Service) Create(ctx context.Context, scope *access.ProjectScope, in CreateInput) (Created, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Created{}, err
	}
	if err := scope.Require(access.CapIntegrationManage); err != nil {
		return Created{}, err
	}
	if err := in.Validate(); err != nil {
		return Created{}, err
	}
	scopes := slices.Clone(in.Scopes)
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	token, hash := newToken()
	now := s.clock.Now()
	p := scope.Principal
	ig := Integration{WorkspaceId: p.WorkspaceId, ProjectId: scope.Project.Id, Name: in.Name, Scopes: scopes,
		Status: StatusActive, TokenHint: token[:len(TokenPrefix)+4], CreatedBy: p.Id, CreatedAt: now, Version: 1}
	ev := audit.New(ctx, p.AuditActor(), audit.IntegrationCreated, "", now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name", "scopes"}
	out, err := s.repo.Create(ctx, ig, hash, ev)
	if err != nil {
		return Created{}, err
	}
	return Created{Integration: out, Token: token}, nil
}

// List pages through a project's integrations.
func (s *Service) List(ctx context.Context, scope *access.ProjectScope, after *pagination.Cursor, limit int) (pagination.Page[Integration], error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return pagination.Page[Integration]{}, err
	}
	if err := scope.Require(access.CapIntegrationManage); err != nil {
		return pagination.Page[Integration]{}, err
	}
	rows, err := s.repo.List(ctx, scope.Principal.WorkspaceId, scope.Project.Id, after, limit+1)
	if err != nil {
		return pagination.Page[Integration]{}, err
	}
	return pagination.NewPage(rows, limit, func(i Integration) pagination.Cursor {
		return pagination.Cursor{Id: i.Id}
	}), nil
}

// Revoke disables an integration; its token stops working immediately.
func (s *Service) Revoke(ctx context.Context, scope *access.ProjectScope, id string) error {
	if err := scope.Principal.RequireUser(); err != nil {
		return err
	}
	if err := scope.Require(access.CapIntegrationManage); err != nil {
		return err
	}
	if !validation.ValidID(id) {
		return fault.NotFound("integrationNotFound")
	}
	now := s.clock.Now()
	ev := audit.New(ctx, scope.AuditActor(), audit.IntegrationRevoked, "integration/"+id, now)
	ev.ProjectId = scope.Project.Id
	err := s.repo.Revoke(ctx, scope.Principal.WorkspaceId, scope.Project.Id, id, now, ev)
	if errors.Is(err, ErrNotFound) {
		return fault.NotFound("integrationNotFound")
	}
	return err
}

// UpdateInput is an integration PATCH. Only the name can change; scopes
// are fixed at creation (issue a new credential to change them).
type UpdateInput struct {
	Name *string
}

// Update renames an active integration. The token, scopes and status are
// unchanged; the new name is used from the integration's next request.
func (s *Service) Update(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64, in UpdateInput) (Integration, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Integration{}, err
	}
	if err := scope.Require(access.CapIntegrationManage); err != nil {
		return Integration{}, err
	}
	var v validation.Collector
	v.OptionalString(validation.IntegrationName, "name", in.Name)
	if in.Name == nil {
		v.Add("name", validation.CodeRequired, "Send the new name.")
	}
	if err := v.Err(); err != nil {
		return Integration{}, err
	}
	if !validation.ValidID(id) {
		return Integration{}, fault.NotFound("integrationNotFound")
	}
	now := s.clock.Now()
	ev := audit.New(ctx, scope.AuditActor(), audit.IntegrationUpdated, "integration/"+id, now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name"}
	out, err := s.repo.Rename(ctx, scope.Principal.WorkspaceId, scope.Project.Id, id, *in.Name, expectedVersion, ev)
	switch {
	case errors.Is(err, ErrNotFound):
		return Integration{}, fault.NotFound("integrationNotFound")
	case errors.Is(err, ErrVersionConflict):
		return Integration{}, fault.Conflict("versionConflict", "This integration was changed by someone else. Reload and try again.")
	case errors.Is(err, ErrRevoked):
		return Integration{}, fault.Conflict("integrationRevoked", "A revoked integration cannot be changed.")
	}
	return out, err
}

// LooksLikeToken reports whether a bearer value is an integration token.
func LooksLikeToken(raw string) bool { return strings.HasPrefix(raw, TokenPrefix) }

// Authenticate resolves an integration token into a principal. Revoked
// credentials and inactive projects are rejected on every request.
func (s *Service) Authenticate(ctx context.Context, raw string) (access.Principal, error) {
	invalid := fault.Unauthenticated("invalidToken", "The API token is not valid.")
	if !LooksLikeToken(raw) || len(raw) > 128 {
		return access.Principal{}, invalid
	}
	cred, err := s.repo.FindByTokenHash(ctx, hashToken(raw))
	if errors.Is(err, ErrNotFound) {
		return access.Principal{}, invalid
	}
	if err != nil {
		return access.Principal{}, err
	}
	ig := cred.Integration
	if ig.Status != StatusActive {
		return access.Principal{}, invalid
	}
	active, err := s.repo.ProjectActive(ctx, ig.WorkspaceId, ig.ProjectId)
	if err != nil {
		return access.Principal{}, err
	}
	if !active {
		return access.Principal{}, invalid
	}
	_ = s.repo.TouchLastUsed(ctx, cred, s.clock.Now(), time.Minute)
	return access.Principal{Kind: access.KindIntegration, Id: ig.Id, WorkspaceId: ig.WorkspaceId, Name: ig.Name,
		ProjectId: ig.ProjectId, Scopes: slices.Clone(ig.Scopes)}, nil
}

func newToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
