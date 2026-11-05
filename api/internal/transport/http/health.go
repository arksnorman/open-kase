package http

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Check is one readiness dependency. Required checks must pass for the
// service to be ready; optional checks report degraded state only.
type Check struct {
	Name     string
	Required bool
	Run      func(ctx context.Context) error
}

// Readiness evaluates checks with a short timeout and caches the result
// briefly so probes cannot overload MongoDB.
type Readiness struct {
	log     *slog.Logger
	checks  []Check
	timeout time.Duration
	ttl     time.Duration

	mu       sync.Mutex
	cachedAt time.Time
	cached   ReadyReport
}

// ReadyReport is the readiness result. It intentionally exposes check names
// and states only; error details go to the log.
type ReadyReport struct {
	Ready  bool              `json:"-"`
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// NewReadiness returns a readiness evaluator.
func NewReadiness(log *slog.Logger, checks ...Check) *Readiness {
	return &Readiness{log: log, checks: checks, timeout: 3 * time.Second, ttl: time.Second}
}

// Evaluate runs all checks (or returns a result cached within the TTL).
func (r *Readiness) Evaluate(ctx context.Context) ReadyReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cachedAt.IsZero() && time.Since(r.cachedAt) < r.ttl {
		return r.cached
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rep := ReadyReport{Ready: true, Checks: map[string]string{}}
	for _, ch := range r.checks {
		if err := ch.Run(ctx); err != nil {
			if ch.Required {
				rep.Ready = false
				rep.Checks[ch.Name] = "fail"
				r.log.Warn("readiness check failed", "check", ch.Name, "error", err.Error())
			} else {
				rep.Checks[ch.Name] = "degraded"
				r.log.Warn("optional dependency degraded", "check", ch.Name, "error", err.Error())
			}
			continue
		}
		rep.Checks[ch.Name] = "ok"
	}
	rep.Status = "ready"
	if !rep.Ready {
		rep.Status = "notReady"
	}
	r.cached, r.cachedAt = rep, time.Now()
	return rep
}

func (r *Readiness) handle(c *gin.Context) {
	rep := r.Evaluate(c.Request.Context())
	status := http.StatusOK
	if !rep.Ready {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, rep)
}
