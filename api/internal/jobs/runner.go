package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
)

// Outcome is what a handler decided for one attempt.
type Outcome int

const (
	// Done: the work finished (including deliberate skips).
	Done Outcome = iota
	// Retry: the attempt failed and may succeed later.
	Retry
	// Dead: retrying will not help; dead-letter the job.
	Dead
)

// Result is a handler's decision. Commit, when set, runs inside the fenced
// completion transaction with the final Finish (a Retry becomes StateDead
// when the attempt budget is spent), so delivery records and the job state
// change atomically and only while the lease is still current.
type Result struct {
	Outcome    Outcome
	Delay      time.Duration // Retry: zero uses the runner's backoff
	ErrorClass string
	Error      string
	Commit     func(ctx context.Context, f Finish) error
}

// Handler runs one attempt of a job. It must keep external side effects
// outside database transactions and use Store.Fenced with the lease for any
// intermediate write.
type Handler func(ctx context.Context, j Job, l Lease) Result

// Config tunes a Runner.
type Config struct {
	Owner       string
	Concurrency int
	LeaseTTL    time.Duration
	Poll        time.Duration
	// Backoff returns the delay after failed attempt n (1-based).
	Backoff func(attempt int) time.Duration
	Now     func() time.Time
	Log     *slog.Logger
	Waker   Waker
	// Grace is how long attempts in progress may run on once Run's context
	// ends (shutdown); zero uses DefaultGrace.
	Grace time.Duration
}

// DefaultGrace lets an attempt in progress (an SMTP or webhook send)
// finish at shutdown. It runs alongside the HTTP server's 20 s drain and
// fits the shipped stop timeouts (Compose 30 s, systemd 45 s).
const DefaultGrace = 15 * time.Second

// DefaultSchedule is the retry delay after each failed attempt: 1 min,
// 5 min, 30 min, 2 h, 12 h (±20 % jitter).
var DefaultSchedule = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}

// DefaultBackoff applies DefaultSchedule with ±20 % jitter.
func DefaultBackoff(attempt int) time.Duration {
	i := min(max(attempt-1, 0), len(DefaultSchedule)-1)
	base := DefaultSchedule[i]
	return time.Duration(float64(base) * (0.8 + 0.4*rand.Float64()))
}

// DeadHook runs in the completion transaction when the runner itself
// dead-letters a job of its kind without a handler result to commit: the
// attempt budget was spent by attempts whose lease expired (worker killed,
// completion failed) or by panics. It lets the owner of a companion record
// (a delivery, a scheduled send, ...) mark it failed, so the record never stays
// "pending" behind a dead job.
type DeadHook func(ctx context.Context, j Job, f Finish) error

// Runner is a bounded pool of workers acquiring jobs from a Store.
type Runner struct {
	store    Store
	cfg      Config
	mu       sync.RWMutex
	handlers map[string]Handler
	dead     map[string]DeadHook
}

// NewRunner returns a runner; register handlers before Run.
func NewRunner(store Store, cfg Config) *Runner {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 2 * time.Minute
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.Backoff == nil {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Owner == "" {
		cfg.Owner = "worker"
	}
	if cfg.Grace <= 0 {
		cfg.Grace = DefaultGrace
	}
	return &Runner{store: store, cfg: cfg, handlers: map[string]Handler{}, dead: map[string]DeadHook{}}
}

// Handle registers the handler for a job kind.
func (r *Runner) Handle(kind string, h Handler) {
	r.mu.Lock()
	r.handlers[kind] = h
	r.mu.Unlock()
}

// OnDead registers the dead-letter hook for a job kind (see DeadHook).
func (r *Runner) OnDead(kind string, h DeadHook) {
	r.mu.Lock()
	r.dead[kind] = h
	r.mu.Unlock()
}

// Kinds returns the registered kinds.
func (r *Runner) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Run processes jobs with Concurrency workers until ctx is cancelled. No
// job is taken after that; attempts in progress may finish within the
// grace period (Config.Grace) and are then cancelled: a handler that
// returns because of it records a failed attempt (a retry after the
// backoff, or dead-lettered on its last attempt). Their leases are renewed
// meanwhile, and their results commit only while the lease holds. An
// attempt of a process that dies without returning keeps its lease until
// it expires; the job is then resumed by any worker.
func (r *Runner) Run(ctx context.Context) {
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(r.cfg.Grace, cancel) })
	defer stop()
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Concurrency; i++ {
		wg.Go(func() { r.loop(ctx, hctx, i) })
	}
	wg.Wait()
}

// loop takes jobs while ctx lasts and runs them under hctx, which outlives
// ctx by the grace period.
func (r *Runner) loop(ctx, hctx context.Context, n int) {
	t := time.NewTimer(0)
	defer t.Stop()
	var wake <-chan struct{}
	if r.cfg.Waker != nil {
		wake = r.cfg.Waker.C()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
		worked, err := r.runOnce(ctx, hctx)
		if err != nil && ctx.Err() == nil {
			r.cfg.Log.Warn("job acquisition failed", "worker", n, "error", err.Error())
		}
		if worked {
			t.Reset(0)
		} else {
			t.Reset(r.cfg.Poll)
		}
	}
}

// RunOnce acquires and processes at most one job. It reports whether a job
// was processed.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) { return r.runOnce(ctx, ctx) }

// runOnce acquires a job under ctx and runs it under hctx.
func (r *Runner) runOnce(ctx, hctx context.Context) (bool, error) {
	j, ok, err := r.store.Acquire(ctx, r.cfg.Owner, r.Kinds(), r.cfg.Now(), r.cfg.LeaseTTL)
	if err != nil || !ok {
		return false, err
	}
	r.process(hctx, j)
	return true, nil
}

func (r *Runner) process(ctx context.Context, j Job) {
	l := Lease{JobId: j.Id, Owner: r.cfg.Owner, Generation: j.Generation}
	started := r.cfg.Now()
	log := r.cfg.Log.With("jobId", j.Id, "kind", j.Kind, "attempt", j.Attempts, "generation", j.Generation)

	var res Result
	if j.Attempts > j.MaxAttempts {
		res = Result{Outcome: Dead, ErrorClass: "attemptsExhausted", Error: "the last attempt's lease expired and no attempts remain"}
	} else {
		r.mu.RLock()
		h := r.handlers[j.Kind]
		r.mu.RUnlock()
		if h == nil {
			res = Result{Outcome: Dead, ErrorClass: "noHandler", Error: "no handler for job kind " + j.Kind}
		} else {
			res = r.runHandler(ctx, h, j, l)
		}
	}

	f := Finish{Attempt: Attempt{Number: j.Attempts, Owner: r.cfg.Owner, Generation: j.Generation, StartedAt: started,
		EndedAt: r.cfg.Now(), ErrorClass: res.ErrorClass, Error: truncate(res.Error, 500)}}
	switch res.Outcome {
	case Done:
		f.State, f.Attempt.Outcome = StateSucceeded, "succeeded"
	case Retry:
		if j.Attempts >= j.MaxAttempts {
			f.State, f.Attempt.Outcome = StateDead, "dead"
		} else {
			delay := res.Delay
			if delay <= 0 {
				delay = r.cfg.Backoff(j.Attempts)
			}
			f.State, f.Attempt.Outcome, f.RunAt = StateQueued, "retry", r.cfg.Now().Add(delay)
		}
	default:
		f.State, f.Attempt.Outcome = StateDead, "dead"
	}
	var also func(context.Context) error
	if res.Commit != nil {
		also = func(ctx context.Context) error { return res.Commit(ctx, f) }
	} else if f.State == StateDead {
		r.mu.RLock()
		hook := r.dead[j.Kind]
		r.mu.RUnlock()
		if hook != nil {
			also = func(ctx context.Context) error { return hook(ctx, j, f) }
		}
	}
	// Completion must not be cut short by shutdown once the work is done.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := r.store.Complete(cctx, l, f, also)
	switch {
	case errors.Is(err, ErrLeaseLost):
		log.Warn("job result discarded: lease was lost to another worker")
	case err != nil:
		log.Warn("job completion failed; the job will run again after its lease expires", "error", err.Error())
	case f.State == StateDead:
		log.Warn("job dead-lettered", "errorClass", res.ErrorClass)
	case f.State == StateQueued:
		log.Info("job will retry", "errorClass", res.ErrorClass, "runAt", f.RunAt)
	}
}

// runHandler runs h while renewing the lease; losing the lease cancels the
// handler's context.
func (r *Runner) runHandler(ctx context.Context, h Handler, j Job, l Lease) (res Result) {
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(r.cfg.LeaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-hctx.Done():
				return
			case <-t.C:
				if err := r.store.Extend(hctx, l, r.cfg.Now().Add(r.cfg.LeaseTTL)); errors.Is(err, ErrLeaseLost) {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		if p := recover(); p != nil {
			res = Result{Outcome: Retry, ErrorClass: "panic", Error: fmt.Sprint(p)}
		}
	}()
	return h(hctx, j, l)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
