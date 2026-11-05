// Package installation implements idempotent first-time setup: the
// workspace, default settings, built-in statuses, default request types, the
// deployment's single project (with its Triage group) and the first
// administrator.
package installation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// ErrKeyMismatch means the loaded master key cannot decrypt this
// installation's key check: the wrong key file is configured.
var ErrKeyMismatch = errors.New("installation: master key does not match this installation")

// RequestType is a default request type to seed. Request type is independent
// of priority.
type RequestType struct {
	Code  string
	Name  string
	Order int
}

// DefaultRequestTypes returns the configurable starting request types.
func DefaultRequestTypes() []RequestType {
	return []RequestType{
		{Code: "issue", Name: "Issue", Order: 10},
		{Code: "question", Name: "Question", Order: 20},
		{Code: "serviceRequest", Name: "Service request", Order: 30},
		{Code: "featureRequest", Name: "Feature request", Order: 40},
	}
}

// HoldReason is a hold reason seeded at init (admins change the list later).
type HoldReason struct {
	Name    string
	MaxDays int
}

// DefaultHoldReasons returns the generic starting hold reasons.
func DefaultHoldReasons() []HoldReason {
	return []HoldReason{
		{Name: "Waiting on Engineering", MaxDays: 5},
		{Name: "Waiting on vendor / third party", MaxDays: 5},
		{Name: "Waiting on another team", MaxDays: 3},
		{Name: "Scheduled maintenance or change", MaxDays: 14},
		{Name: "Waiting for deployment / release", MaxDays: 7},
		{Name: "Under investigation", MaxDays: 3},
	}
}

// The single project's internal key and name. Neither is shown to users or
// integrators: tickets are identified by number (owner decision
// 2026-10-31); the stored project record still needs both.
const (
	DefaultProjectKey  = "SUP"
	DefaultProjectName = "Support"
)

// State describes what already exists.
type State struct {
	Initialized   bool
	WorkspaceId   string
	WorkspaceName string
	KeyCheck      string
	AdminExists   bool
}

// AdminSeed is the first administrator to create.
type AdminSeed struct {
	Email           string
	EmailNormalized string
	// Username is derived from the email address (the first user cannot
	// collide with another).
	Username     string
	Name         users.PersonName
	PasswordHash string
}

// ProjectSeed is the project to create when the workspace has none.
type ProjectSeed struct {
	Key  string
	Name string
}

// Seed is everything initialization ensures exists. The repository applies
// it in one transaction and only inserts missing records.
type Seed struct {
	WorkspaceName string
	KeyCheck      string
	Settings      settings.Settings
	Statuses      []statuses.Definition
	RequestTypes  []RequestType
	HoldReasons   []HoldReason
	Project       ProjectSeed
	Admin         *AdminSeed
	Actor         audit.Actor
	Now           time.Time
}

// Result reports what initialization created.
type Result struct {
	WorkspaceId         string
	WorkspaceCreated    bool
	SettingsCreated     bool
	StatusesCreated     int
	RequestTypesCreated int
	ProjectCreated      bool
	AdminCreated        bool
}

// Repository persists installation records.
type Repository interface {
	State(ctx context.Context) (State, error)
	Seed(ctx context.Context, seed Seed) (Result, error)
}

// KeyChecker creates and verifies the master-key check value.
type KeyChecker interface {
	NewKeyCheck() (string, error)
	VerifyKeyCheck(check string) error
}

// PasswordHasher hashes administrator passwords.
type PasswordHasher func(password string) (string, error)

// Service runs installation commands.
type Service struct {
	repo   Repository
	keys   KeyChecker
	hasher PasswordHasher
	clock  clock.Clock
}

// NewService constructs the installation service.
func NewService(repo Repository, keys KeyChecker, hasher PasswordHasher, clk clock.Clock) *Service {
	return &Service{repo: repo, keys: keys, hasher: hasher, clock: clk}
}

// State returns the current installation state and verifies the master key
// against an existing installation.
func (s *Service) State(ctx context.Context) (State, error) {
	st, err := s.repo.State(ctx)
	if err != nil {
		return st, err
	}
	if st.Initialized {
		if err := s.keys.VerifyKeyCheck(st.KeyCheck); err != nil {
			return st, fmt.Errorf("%w: %v", ErrKeyMismatch, err)
		}
	}
	return st, nil
}

// Input is the operator-supplied initialization data. Admin fields are
// required only when no administrator exists yet.
type Input struct {
	WorkspaceName string
	AdminEmail    string
	AdminName     users.PersonName
	AdminPassword string
	Actor         audit.Actor
	// RequireAdminMFA seeds auth.requireAdminMfa for a new installation
	// (init sets it outside development). Existing settings are unchanged.
	RequireAdminMFA bool
}

// Validate checks the input fields relevant to the current state.
func (in Input) Validate(needAdmin bool) error {
	var v validation.Collector
	v.String(validation.WorkspaceName, "workspaceName", in.WorkspaceName)
	if needAdmin {
		v.String(validation.UserEmail, "adminEmail", in.AdminEmail)
		in.AdminName.Trimmed().Validate(&v, "admin")
		v.String(validation.UserPassword, "adminPassword", in.AdminPassword)
	}
	return v.Err()
}

// Initialize ensures the installation exists. Re-running it is safe: existing
// records are kept and only missing defaults are added.
func (s *Service) Initialize(ctx context.Context, in Input) (Result, error) {
	st, err := s.State(ctx)
	if err != nil {
		return Result{}, err
	}
	if in.Actor.Type == "" {
		in.Actor = audit.Operator(audit.SystemInstallation, "installation")
	}
	needAdmin := !st.AdminExists
	if st.Initialized {
		in.WorkspaceName = st.WorkspaceName
	}

	if err := in.Validate(needAdmin); err != nil {
		return Result{}, err
	}
	seed := Seed{
		WorkspaceName: in.WorkspaceName,
		KeyCheck:      st.KeyCheck,
		Settings:      settings.Defaults(),
		Statuses:      statuses.Builtins(),
		RequestTypes:  DefaultRequestTypes(),
		HoldReasons:   DefaultHoldReasons(),
		Project:       ProjectSeed{Key: DefaultProjectKey, Name: DefaultProjectName},
		Actor:         in.Actor,
		Now:           s.clock.Now(),
	}
	seed.Settings.Auth.RequireAdminMFA = in.RequireAdminMFA
	if !st.Initialized {
		if seed.KeyCheck, err = s.keys.NewKeyCheck(); err != nil {
			return Result{}, err
		}
	}
	if needAdmin {
		hash, err := s.hasher(in.AdminPassword)
		if err != nil {
			return Result{}, fmt.Errorf("hash admin password: %w", err)
		}
		seed.Admin = &AdminSeed{
			Email:           in.AdminEmail,
			EmailNormalized: users.NormalizeEmail(in.AdminEmail),
			Username:        users.UsernameBase(in.AdminEmail),
			Name:            in.AdminName.Trimmed(),
			PasswordHash:    hash,
		}
	}
	return s.repo.Seed(ctx, seed)
}
