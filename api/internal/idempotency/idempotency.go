// Package idempotency defines replay-safe command records for create
// requests that carry an Idempotency-Key. Keys are scoped to the principal
// and the operation; reusing a key with a different request is rejected.
//
// Records are reserved before the command runs and completed after it
// commits. A process that dies between commit and completion leaves a
// pending reservation; after its lease expires a retry runs the command
// again, and the command's own uniqueness rules (project key, email, group
// name, membership tuple) reject a duplicate.
package idempotency

import (
	"context"
	"errors"
	"time"
)

// Errors.
var (
	ErrMismatch   = errors.New("idempotency: key reused with a different request")
	ErrInProgress = errors.New("idempotency: request with this key is still in progress")
)

// Record is a completed response that can be replayed.
type Record struct {
	Status   int
	Body     []byte
	Location string
}

// Store persists idempotency records.
type Store interface {
	// Begin reserves (scope, key) for requestHash. It returns a completed
	// record to replay, or nil when the caller should execute the command.
	Begin(ctx context.Context, scope, key, requestHash string, now time.Time) (*Record, error)
	// Complete stores the response of a successful command.
	Complete(ctx context.Context, scope, key string, rec Record, now time.Time) error
	// Abandon releases a reservation after a failed command so it can be retried.
	Abandon(ctx context.Context, scope, key string) error
}

// Lease bounds how long a pending reservation blocks retries.
const Lease = time.Minute

// Retention is how long completed records are kept for replay.
const Retention = 24 * time.Hour

// outcomeTimeout bounds the write that records a command's outcome.
const outcomeTimeout = 10 * time.Second

// OutcomeContext is the context for Complete and Abandon. The command has
// already run, so the write is detached from the request's cancellation
// (bounded by its own timeout): a client that disconnects right after the
// commit must not leave the reservation pending, which made its retry
// answer idempotencyInProgress for up to a minute and then run the
// command a second time (pass-3 review OC-OPS-09).
func OutcomeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), outcomeTimeout)
}

// Ref identifies the reserved idempotency record of the current request.
// Commands that can store their idempotency marker inside their own
// business transaction (ticket creation) read it from the context.
type Ref struct {
	Scope string
	Key   string
}

type refKey struct{}

// WithRef attaches the request's reserved idempotency record to ctx.
func WithRef(ctx context.Context, ref Ref) context.Context {
	return context.WithValue(ctx, refKey{}, ref)
}

// RefFrom returns the reserved record for this request, if any.
func RefFrom(ctx context.Context) (Ref, bool) {
	r, ok := ctx.Value(refKey{}).(Ref)
	return r, ok && r.Key != ""
}
