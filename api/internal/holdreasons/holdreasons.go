// Package holdreasons manages the reasons a ticket can be put on hold for.
// Each reason has a name and a maximum number of days: a ticket put on hold
// for the reason returns to Open after exactly that many days. Reasons are
// managed by admins and by integrations with the holdReasons scopes; every
// staff member can read them. The package and the stored collection are
// named "holdReasons".
package holdreasons

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// MaxReasons bounds the hold reasons of one project.
const MaxReasons = 100

// Reason is a hold reason.
type Reason struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	Name        string
	MaxDays     int
	// InUse is the number of tickets currently on hold with this reason. It
	// is filled by List for callers who manage hold reasons only.
	InUse     int64
	CreatedBy string
	CreatedAt time.Time
	Version   int64
}

// NormalizeName is the key of the per-project unique name.
func NormalizeName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// Repository errors.
var (
	ErrNotFound = errors.New("holdReasons: not found")
	ErrConflict = errors.New("holdReasons: version conflict")
	ErrExists   = errors.New("holdReasons: name exists")
	ErrLimit    = errors.New("holdReasons: limit reached")
	// ErrInUse means a ticket is currently on hold with the reason.
	ErrInUse = errors.New("holdReasons: in use")
)

// Repository persists hold reasons.
type Repository interface {
	// List returns the project's reasons by name; withUsage fills InUse.
	List(ctx context.Context, projectID string, withUsage bool) ([]Reason, error)
	Get(ctx context.Context, projectID, id string) (Reason, error)
	Create(ctx context.Context, r Reason, ev audit.Event) (Reason, error)
	Update(ctx context.Context, r Reason, expectedVersion int64, ev audit.Event) (Reason, error)
	// Delete refuses with ErrInUse while a ticket is on hold with the reason.
	Delete(ctx context.Context, r Reason, expectedVersion int64, ev audit.Event) error
}

// Service implements hold reason management.
type Service struct {
	repo  Repository
	clock clock.Clock
}

// NewService returns the hold reasons service.
func NewService(repo Repository, clk clock.Clock) *Service { return &Service{repo: repo, clock: clk} }

func errNotFound() error { return fault.NotFound("holdReasonNotFound") }

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return errNotFound()
	case errors.Is(err, ErrConflict):
		return fault.Conflict("versionConflict", "The hold reason was changed by someone else. Reload and try again.")
	case errors.Is(err, ErrExists):
		return fault.Conflict("holdReasonExists", "A hold reason with this name already exists.")
	case errors.Is(err, ErrLimit):
		return fault.Conflict("holdReasonLimitReached", "A project can have at most 100 hold reasons.")
	case errors.Is(err, ErrInUse):
		return fault.Conflict("holdReasonInUse", "A ticket is on hold with this reason. Wait for it to come off hold, or change its status first.")
	}
	return err
}

// Input creates or changes a hold reason.
type Input struct {
	Name    *string
	MaxDays *int
}

func validate(r Reason) error {
	var v validation.Collector
	v.String(validation.HoldReasonName, "name", r.Name)
	v.Int(validation.HoldReasonMaxDays, "maxDays", int64(r.MaxDays))
	return v.Err()
}

// List returns the project's hold reasons (users and tokens with
// holdReasons:read or holdReasons:write; staff tokens with tickets:read).
// inUse counts are reported to callers who manage hold reasons.
func (s *Service) List(ctx context.Context, scope *access.ProjectScope) ([]Reason, error) {
	if err := scope.Require(access.CapHoldReasonRead); err != nil {
		return nil, err
	}
	return s.repo.List(ctx, scope.Project.Id, scope.Can(access.CapHoldReasonManage))
}

// Get returns one hold reason.
func (s *Service) Get(ctx context.Context, scope *access.ProjectScope, id string) (Reason, error) {
	if err := scope.Require(access.CapHoldReasonRead); err != nil {
		return Reason{}, err
	}
	if !validation.ValidID(id) {
		return Reason{}, errNotFound()
	}
	r, err := s.repo.Get(ctx, scope.Project.Id, id)
	return r, mapErr(err)
}

// Create adds a hold reason (admins; tokens with holdReasons:write).
func (s *Service) Create(ctx context.Context, scope *access.ProjectScope, in Input) (Reason, error) {
	if err := scope.Require(access.CapHoldReasonManage); err != nil {
		return Reason{}, err
	}
	r := Reason{WorkspaceId: scope.Principal.WorkspaceId, ProjectId: scope.Project.Id, CreatedBy: scope.Principal.Id,
		CreatedAt: s.clock.Now(), Version: 1}
	if in.Name != nil {
		r.Name = strings.TrimSpace(*in.Name)
	}
	if in.MaxDays != nil {
		r.MaxDays = *in.MaxDays
	}
	if err := validate(r); err != nil {
		return Reason{}, err
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.HoldReasonCreated, "", r.CreatedAt)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name", "maxDays"}
	out, err := s.repo.Create(ctx, r, ev)
	return out, mapErr(err)
}

// Update changes a hold reason with If-Match. A new maxDays affects only
// holds started afterwards.
func (s *Service) Update(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64, in Input) (Reason, error) {
	if err := scope.Require(access.CapHoldReasonManage); err != nil {
		return Reason{}, err
	}
	r, err := s.Get(ctx, scope, id)
	if err != nil {
		return Reason{}, err
	}
	var changed []string
	if in.Name != nil && strings.TrimSpace(*in.Name) != r.Name {
		r.Name, changed = strings.TrimSpace(*in.Name), append(changed, "name")
	}
	if in.MaxDays != nil && *in.MaxDays != r.MaxDays {
		r.MaxDays, changed = *in.MaxDays, append(changed, "maxDays")
	}
	if err := validate(r); err != nil {
		return Reason{}, err
	}
	if r.Version != expectedVersion {
		return Reason{}, mapErr(ErrConflict)
	}
	if len(changed) == 0 {
		return r, nil
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.HoldReasonUpdated, "holdReason/"+r.Id, s.clock.Now())
	ev.ProjectId, ev.Changes = scope.Project.Id, changed
	out, err := s.repo.Update(ctx, r, expectedVersion, ev)
	return out, mapErr(err)
}

// Delete removes a hold reason with If-Match. It is refused with
// holdReasonInUse while any ticket is on hold with it; past holds keep the
// reason name on the ticket and in its activity.
func (s *Service) Delete(ctx context.Context, scope *access.ProjectScope, id string, expectedVersion int64) error {
	if err := scope.Require(access.CapHoldReasonManage); err != nil {
		return err
	}
	r, err := s.Get(ctx, scope, id)
	if err != nil {
		return err
	}
	if r.Version != expectedVersion {
		return mapErr(ErrConflict)
	}
	ev := audit.New(ctx, scope.AuditActor(), audit.HoldReasonDeleted, "holdReason/"+r.Id, s.clock.Now())
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name"}
	return mapErr(s.repo.Delete(ctx, r, expectedVersion, ev))
}
