package sla

import (
	"context"
	"strconv"
	"time"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// KindCheck is the job kind of SLA timers. One job is scheduled per cycle,
// generation and check time; it carries the generation it was scheduled
// for and does nothing when the cycle has moved on.
const KindCheck = "sla.check"

// Checker performs a timer check inside the caller's transaction
// (tickets.Service).
type Checker interface {
	CheckSLA(ctx context.Context, instanceID string, generation int64) error
}

// CheckHandler returns the job handler for KindCheck. All writes (breach
// records, outbox events, the next timer) happen in the job's fenced
// completion transaction: a worker whose lease was taken over commits
// nothing, so a breach is announced at most once per cycle generation.
func CheckHandler(c Checker) jobs.Handler {
	return func(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
		id := j.Payload["instanceId"]
		gen, err := strconv.ParseInt(j.Payload["generation"], 10, 64)
		if err != nil || !validation.ValidID(id) {
			return jobs.Result{Outcome: jobs.Dead, ErrorClass: "invalidPayload", Error: "the SLA timer payload is invalid"}
		}
		return jobs.Result{Outcome: jobs.Done, Commit: func(ctx context.Context, f jobs.Finish) error {
			if f.State != jobs.StateSucceeded {
				return nil
			}
			return c.CheckSLA(ctx, id, gen)
		}}
	}
}

// KindStop is the job kind that stops a project's open cycles after SLA
// was turned off (the policy was deactivated). Each job stops one bounded
// batch in its fenced completion transaction and, while more remain,
// enqueues the next part in the same transaction.
const KindStop = "sla.stop"

// StopBatch bounds the cycles one stop job (one transaction) stops.
const StopBatch = 100

// StopRun is one part of a deactivation sweep: the project's open cycles
// that started with a policy version below BeforeVersion are stopped as of
// At (or as of their own last change, when that is later). Stopped counts
// the cycles the earlier parts stopped.
type StopRun struct {
	WorkspaceId   string
	ProjectId     string
	BeforeVersion int64
	At            time.Time
	Part          int
	Stopped       int
}

// StopJob returns the job for one part of a sweep, due at runAt. Keys are
// unique per project, deactivation version and part.
func StopJob(r StopRun, runAt time.Time) jobs.NewJob {
	key := "sla:stop:" + r.ProjectId + ":v" + strconv.FormatInt(r.BeforeVersion, 10)
	if r.Part > 1 {
		key += ":" + strconv.Itoa(r.Part)
	}
	return jobs.NewJob{WorkspaceId: r.WorkspaceId, ProjectId: r.ProjectId, Kind: KindStop, Key: key, RunAt: runAt,
		Payload: map[string]string{"projectId": r.ProjectId, "beforeVersion": strconv.FormatInt(r.BeforeVersion, 10),
			"at": r.At.UTC().Format(time.RFC3339Nano), "part": strconv.Itoa(r.Part), "stopped": strconv.Itoa(r.Stopped)}}
}

func parseStopRun(j jobs.Job) (StopRun, bool) {
	r := StopRun{WorkspaceId: j.WorkspaceId, ProjectId: j.Payload["projectId"]}
	var err error
	if r.BeforeVersion, err = strconv.ParseInt(j.Payload["beforeVersion"], 10, 64); err != nil || r.BeforeVersion < 1 {
		return r, false
	}
	if r.At, err = time.Parse(time.RFC3339Nano, j.Payload["at"]); err != nil {
		return r, false
	}
	if r.Part, err = strconv.Atoi(j.Payload["part"]); err != nil || r.Part < 1 {
		return r, false
	}
	if r.Stopped, err = strconv.Atoi(j.Payload["stopped"]); err != nil || r.Stopped < 0 {
		return r, false
	}
	return r, validation.ValidID(r.ProjectId) && validation.ValidID(r.WorkspaceId)
}

// Stopper stops one part of a sweep inside the caller's transaction
// (tickets.Service).
type Stopper interface {
	StopSLA(ctx context.Context, r StopRun) error
}

// StopHandler returns the job handler for KindStop. A worker whose lease
// was taken over commits nothing; stopping is idempotent (a stopped cycle
// is no longer selected), so a part that runs again does no harm.
func StopHandler(s Stopper) jobs.Handler {
	return func(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
		r, ok := parseStopRun(j)
		if !ok {
			return jobs.Result{Outcome: jobs.Dead, ErrorClass: "invalidPayload", Error: "the SLA stop payload is invalid"}
		}
		return jobs.Result{Outcome: jobs.Done, Commit: func(ctx context.Context, f jobs.Finish) error {
			if f.State != jobs.StateSucceeded {
				return nil
			}
			return s.StopSLA(ctx, r)
		}}
	}
}
