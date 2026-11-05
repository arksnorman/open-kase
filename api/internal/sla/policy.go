package sla

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Policy is the deployment's SLA (one per project; there is one project).
// Each save increments Version; ticket cycles keep the snapshot of the
// version they started with, so a change applies to new cycles (new
// tickets) only. Turning it off stops the open cycles. Version 0 is the
// default that was never saved (off, no targets, warning at 80 %).
type Policy struct {
	Id             string
	WorkspaceId    string
	ProjectId      string
	Active         bool
	Targets        [4]Targets
	WarningPercent int
	Version        int64
	// DeactivatedVersion is the version of the latest save that turned SLA
	// off (0: never) and DeactivatedAt its time. A cycle that started with
	// an older version is stopped as of that time, also after SLA was
	// turned on again (that is not retroactive).
	DeactivatedVersion int64
	DeactivatedAt      *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Stops reports whether SLA was turned off after a cycle with this
// snapshot started: SLA is off, or it was turned off since.
// Such a cycle must announce nothing and is stopped.
func (p Policy) Stops(s Snapshot) bool {
	return !p.Active || s.PolicyVersion < p.DeactivatedVersion
}

// StopTime is when a cycle with this snapshot stopped (see Stops): the
// deactivation time, or fallback when there is none.
func (p Policy) StopTime(fallback time.Time) time.Time {
	if p.DeactivatedAt != nil {
		return *p.DeactivatedAt
	}
	return fallback
}

// Snapshot returns the SLA as recorded on a cycle.
func (p Policy) Snapshot() Snapshot {
	s := Snapshot{PolicyId: p.Id, PolicyVersion: p.Version, Targets: p.Targets,
		WarningPercent: p.WarningPercent}
	return Instance{Snapshot: s}.Clone().Snapshot
}

// Repository errors.
var (
	ErrNotFound = errors.New("sla: not found")
	ErrConflict = errors.New("sla: version conflict")
)

// Repository persists the SLA.
type Repository interface {
	// GetPolicy returns ErrNotFound when the SLA was never saved.
	GetPolicy(ctx context.Context, projectID string) (Policy, error)
	// SavePolicy inserts (expectedVersion 0) or replaces the SLA with
	// the version check; ErrConflict on a stale version or a concurrent
	// first insert. A non-nil stop job is enqueued in the same transaction.
	SavePolicy(ctx context.Context, p Policy, expectedVersion int64, ev audit.Event, stop *jobs.NewJob) (Policy, error)
}

// Service manages the SLA.
type Service struct {
	repo  Repository
	clock clock.Clock
	wake  func(context.Context)
}

// NewService returns the SLA service; wake (may be nil) nudges the job
// workers after turning SLA off enqueued the stop job.
func NewService(repo Repository, clk clock.Clock, wake func(context.Context)) *Service {
	return &Service{repo: repo, clock: clk, wake: wake}
}

func errVersionConflict() error {
	return fault.Conflict("versionConflict", "The SLA was changed by someone else. Reload and try again.")
}

// defaultPolicy is the SLA before it is first saved.
func defaultPolicy(projectID string) Policy {
	return Policy{ProjectId: projectID, WarningPercent: 80}
}

// Get returns the SLA (any staff member); the default (version 0) when it
// was never saved.
func (s *Service) Get(ctx context.Context, scope *access.ProjectScope) (Policy, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Policy{}, err
	}
	if err := scope.Require(access.CapTicketRead); err != nil {
		return Policy{}, err
	}
	p, err := s.repo.GetPolicy(ctx, scope.Project.Id)
	if errors.Is(err, ErrNotFound) {
		return defaultPolicy(scope.Project.Id), nil
	}
	return p, err
}

// ActiveSnapshot returns the snapshot new cycles start with, or nil when
// SLA is off (the default).
func (s *Service) ActiveSnapshot(ctx context.Context, projectID string) (*Snapshot, error) {
	p, err := s.repo.GetPolicy(ctx, projectID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !p.Active {
		return nil, nil
	}
	snap := p.Snapshot()
	return &snap, nil
}

// TargetsInput is one priority's first-response target in minutes (nil = no
// target).
type TargetsInput struct {
	FirstResponseMinutes *int
}

// PolicyInput replaces the whole SLA.
type PolicyInput struct {
	Active         bool
	WarningPercent *int
	Targets        map[string]TargetsInput // "1" … "4"
}

func (in PolicyInput) targets(v *validation.Collector) [4]Targets {
	var targets [4]Targets
	for key := range in.Targets {
		if key != "1" && key != "2" && key != "3" && key != "4" {
			v.Add("targets."+key, validation.CodeUnknownField, "Use the priorities 1 to 4 as keys.")
		}
	}
	for p := 1; p <= 4; p++ {
		ti, ok := in.Targets[strconv.Itoa(p)]
		if !ok || ti.FirstResponseMinutes == nil {
			continue
		}
		m := *ti.FirstResponseMinutes
		v.Int(validation.SLATargetMinutes, "targets."+strconv.Itoa(p)+".firstResponseMinutes", int64(m))
		targets[p-1] = Targets{FirstResponseMinutes: m}
	}
	return targets
}

// Put saves the SLA (administrators).
// expectedVersion must be 0 to create and the current version to replace.
func (s *Service) Put(ctx context.Context, scope *access.ProjectScope, expectedVersion int64, in PolicyInput) (Policy, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Policy{}, err
	}
	if err := scope.Require(access.CapProjectManage); err != nil {
		return Policy{}, err
	}
	var v validation.Collector
	targets := in.targets(&v)
	warn := 80
	if in.WarningPercent != nil {
		warn = *in.WarningPercent
		v.Int(validation.SLAWarningPercent, "warningPercent", int64(warn))
	}
	if err := v.Err(); err != nil {
		return Policy{}, err
	}
	cur, err := s.repo.GetPolicy(ctx, scope.Project.Id)
	switch {
	case errors.Is(err, ErrNotFound):
		if expectedVersion != 0 {
			return Policy{}, errVersionConflict()
		}
	case err != nil:
		return Policy{}, err
	case expectedVersion == 0:
		return Policy{}, fault.New(fault.KindPrecondition, "preconditionRequired", "The SLA was saved before; send If-Match with its version.")
	case cur.Version != expectedVersion:
		return Policy{}, errVersionConflict()
	}
	now := s.clock.Now()
	p := Policy{Id: cur.Id, WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, Active: in.Active,
		Targets: targets, WarningPercent: warn,
		Version: expectedVersion + 1, DeactivatedVersion: cur.DeactivatedVersion, DeactivatedAt: cur.DeactivatedAt,
		CreatedAt: cur.CreatedAt, UpdatedAt: now}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.SLAUpdated, "", now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"active", "targets", "warningPercent"}
	// Turning SLA off stops the open cycles: the stop job is committed with
	// the SLA and works through them in bounded batches. Until it reaches a
	// cycle, that cycle's timers re-check the SLA and announce nothing.
	var stop *jobs.NewJob
	if cur.Active && !p.Active {
		p.DeactivatedVersion, p.DeactivatedAt = p.Version, &now
		j := StopJob(StopRun{WorkspaceId: p.WorkspaceId, ProjectId: p.ProjectId, BeforeVersion: p.Version, At: now, Part: 1}, now)
		stop = &j
	}
	out, err := s.repo.SavePolicy(ctx, p, expectedVersion, ev, stop)
	if errors.Is(err, ErrConflict) {
		return Policy{}, errVersionConflict()
	}
	if err == nil && stop != nil && s.wake != nil {
		s.wake(ctx)
	}
	return out, err
}
