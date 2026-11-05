// Package jobs runs durable background work stored in MongoDB.
//
// A job is acquired with an atomic conditional update that sets
// leaseOwner, leaseUntil and increments a fencing generation. Every later
// write by the worker (intermediate state, completion, retry, dead-letter)
// must match the job's current generation and owner, so a worker whose
// lease expired and was taken over cannot commit stale results. A worker
// that crashes simply stops renewing its lease; another worker acquires the
// job after leaseUntil and runs it again (at-least-once).
//
// Jobs carry identifiers and constrained metadata, never message bodies.
// Unique logical keys make enqueueing idempotent. Redis is never involved
// in correctness; an optional Waker only shortens polling latency.
package jobs

import (
	"context"
	"errors"
	"time"
)

// State is a job's lifecycle state.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	// StateDead is the dead-letter state: retries are exhausted or the
	// failure is permanent. An operator may retry it manually.
	StateDead State = "dead"
)

// DefaultMaxAttempts is 1 attempt plus 5 retries.
const DefaultMaxAttempts = 6

// MaxHistory bounds the attempt history stored on a job.
const MaxHistory = 20

// Job is a durable unit of work.
type Job struct {
	Id          string
	WorkspaceId string
	ProjectId   string
	Kind        string
	Key         string
	State       State
	// Payload holds identifiers and small constrained values only.
	Payload     map[string]string
	Attempts    int
	MaxAttempts int
	RunAt       time.Time
	LeaseOwner  string
	LeaseUntil  *time.Time
	Generation  int64
	LastError   string
	LastClass   string
	History     []Attempt
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// Attempt is one recorded run. It never contains secrets or bodies.
type Attempt struct {
	Number     int
	Owner      string
	Generation int64
	StartedAt  time.Time
	EndedAt    time.Time
	Outcome    string // "succeeded", "retry", "dead", "leaseExpired"
	ErrorClass string
	Error      string
}

// NewJob describes a job to enqueue.
type NewJob struct {
	Id          string // optional pre-allocated ID (ObjectID hex)
	WorkspaceId string
	ProjectId   string
	Kind        string
	// Key is the unique logical key, e.g. "email:<event>:user:<id>".
	Key         string
	Payload     map[string]string
	RunAt       time.Time
	MaxAttempts int
}

// Lease proves ownership of a running job at one fencing generation.
type Lease struct {
	JobId      string
	Owner      string
	Generation int64
}

// Errors.
var (
	// ErrLeaseLost means the job is no longer held by this lease: another
	// worker took it over after expiry, or it was completed or retried.
	ErrLeaseLost = errors.New("jobs: lease lost (stale worker)")
	ErrNotFound  = errors.New("jobs: not found")
	// ErrNotRetryable means a manual retry was requested for a job that is
	// not dead-lettered or completed.
	ErrNotRetryable = errors.New("jobs: job is not dead-lettered")
)

// Finish is the final or rescheduling write for an attempt.
type Finish struct {
	State   State     // StateSucceeded, StateQueued (retry later) or StateDead
	RunAt   time.Time // next attempt time for StateQueued
	Attempt Attempt
}

// Filter selects jobs for listing.
type Filter struct {
	State State
	Kind  string
	Limit int
}

// Store persists jobs. Implementations must make Acquire atomic and fence
// every write after acquisition on (id, owner, generation, running).
type Store interface {
	// Enqueue inserts jobs whose keys do not exist yet and returns how many
	// were created. Existing keys are left untouched (idempotent).
	Enqueue(ctx context.Context, jobs ...NewJob) (int, error)
	// Acquire leases one due job of the given kinds: a queued job whose
	// runAt has passed, or a running job whose lease expired.
	Acquire(ctx context.Context, owner string, kinds []string, now time.Time, ttl time.Duration) (Job, bool, error)
	// Extend renews the lease. It returns ErrLeaseLost when fenced out.
	Extend(ctx context.Context, l Lease, until time.Time) error
	// Fenced runs fn in a transaction after proving the lease is current;
	// fn's writes commit only if the lease still holds.
	Fenced(ctx context.Context, l Lease, fn func(ctx context.Context) error) error
	// Complete applies f and runs also (may be nil) in the same fenced
	// transaction.
	Complete(ctx context.Context, l Lease, f Finish, also func(ctx context.Context) error) error
	Get(ctx context.Context, id string) (Job, error)
	List(ctx context.Context, f Filter) ([]Job, error)
	// Requeue makes a dead (or succeeded) job runnable again with a fresh
	// attempt budget; also runs in the same transaction.
	Requeue(ctx context.Context, id string, now time.Time, also func(ctx context.Context, j Job) error) (Job, error)
}

// Waker optionally shortens the polling delay when new work is enqueued.
type Waker interface {
	// Notify signals that jobs were enqueued (best effort).
	Notify(ctx context.Context)
	// C delivers wake-up signals.
	C() <-chan struct{}
}

// LocalWaker is an in-process Waker. It is also the fallback when Redis is
// not available.
type LocalWaker struct{ ch chan struct{} }

// NewLocalWaker returns a waker with a one-slot buffer.
func NewLocalWaker() *LocalWaker { return &LocalWaker{ch: make(chan struct{}, 1)} }

// Notify wakes one waiting worker loop without blocking.
func (w *LocalWaker) Notify(context.Context) {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// C returns the signal channel.
func (w *LocalWaker) C() <-chan struct{} { return w.ch }
