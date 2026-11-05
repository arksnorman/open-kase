package http

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/ratelimit"
)

// RateLimit is one request-rate class: PerMinute sustained requests per key
// with bursts of Burst. PerMinute < 0 disables the class.
type RateLimit struct {
	PerMinute int
	Burst     int
}

// Rate-limit classes. Limits are per principal (user or integration) on
// authenticated routes and per client IP on the unauthenticated /auth
// routes. They are in-process (one API process, owner override O3) and do
// not depend on Redis. Login, MFA and password-reset failures are
// additionally throttled per account and IP in MongoDB (internal/auth).
const (
	LimitAuth           = "auth"
	LimitTicketCreate   = "ticketCreate"
	LimitMessagePost    = "messagePost"
	LimitUpload         = "upload"
	LimitSearch         = "search"
	LimitConnectionTest = "connectionTest"
)

// DefaultRateLimits are sized for interactive staff use and integrations
// that create tickets from mail gateways or monitoring; see docs/operations.
var DefaultRateLimits = map[string]RateLimit{
	LimitAuth:           {PerMinute: 60, Burst: 30},
	LimitTicketCreate:   {PerMinute: 120, Burst: 60},
	LimitMessagePost:    {PerMinute: 120, Burst: 60},
	LimitUpload:         {PerMinute: 60, Burst: 30},
	LimitSearch:         {PerMinute: 120, Burst: 60},
	LimitConnectionTest: {PerMinute: 6, Burst: 6},
}

// routeLimit maps "METHOD route" to its rate-limit class.
var routeLimit = map[string]string{
	"POST /api/v1/auth/login":                            LimitAuth,
	"POST /api/v1/auth/refresh":                          LimitAuth,
	"POST /api/v1/auth/logout":                           LimitAuth,
	"POST /api/v1/auth/mfa":                              LimitAuth,
	"POST /api/v1/auth/password-reset/request":           LimitAuth,
	"POST /api/v1/auth/password-reset/complete":          LimitAuth,
	"POST /api/v1/auth/invitations/accept":               LimitAuth,
	"POST /api/v1/tickets":                               LimitTicketCreate,
	"POST /api/v1/tickets/:t/follow-up":                  LimitTicketCreate,
	"POST /api/v1/tickets/:t/copy":                       LimitTicketCreate,
	"POST /api/v1/tickets/:t/messages":                   LimitMessagePost,
	"POST /api/v1/tickets/:t/scheduled-messages":         LimitMessagePost,
	"POST /api/v1/tickets/:t/scheduled-messages/:s/send": LimitMessagePost,
	"POST /api/v1/attachments":                           LimitUpload,
	"GET /api/v1/search":                                 LimitSearch,
	"POST /api/v1/settings/smtp/test":                    LimitConnectionTest,
	"POST /api/v1/webhooks/:w/test":                      LimitConnectionTest,
}

// maxRateKeys bounds each class's memory.
const maxRateKeys = 50_000

type rateLimiters map[string]*ratelimit.Limiter

func newRateLimiters(overrides map[string]RateLimit, now func() time.Time) rateLimiters {
	out := rateLimiters{}
	for class, def := range DefaultRateLimits {
		if o, ok := overrides[class]; ok {
			def = o
		}
		if def.PerMinute < 0 {
			continue
		}
		out[class] = ratelimit.New(def.PerMinute, def.Burst, maxRateKeys, now)
	}
	return out
}

// rateLimit applies the route's class, if any. It runs after authenticate
// on authenticated routes (key: principal) and on the /auth group (key:
// client IP; skipped when the client IP cannot be trusted, see throttleIP).
func (a *api) rateLimit(c *gin.Context) {
	class, ok := routeLimit[c.Request.Method+" "+c.FullPath()]
	if !ok {
		c.Next()
		return
	}
	l := a.limiters[class]
	if l == nil {
		c.Next()
		return
	}
	var key string
	if p := principal(c); p.Id != "" {
		key = string(p.Kind) + ":" + p.Id
	} else if ip := throttleIP(c); ip != "" {
		key = "ip:" + ip
	} else {
		c.Next()
		return
	}
	if wait := l.Allow(key); wait > 0 {
		writeError(c, fault.RateLimited("rateLimited", "Too many requests. Retry later.", wait))
		return
	}
	c.Next()
}

// MaxLiveStreamsPerPrincipal bounds concurrent SSE streams per user or
// integration (each holds a goroutine and re-checks access every heartbeat).
const MaxLiveStreamsPerPrincipal = 10

// streamCounter counts open long-lived streams per key.
type streamCounter struct {
	mu   sync.Mutex
	max  int
	open map[string]int
}

func newStreamCounter(limit int) *streamCounter {
	return &streamCounter{max: limit, open: map[string]int{}}
}

func (s *streamCounter) acquire(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[key] >= s.max {
		return false
	}
	s.open[key]++
	return true
}

func (s *streamCounter) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[key] <= 1 {
		delete(s.open, key)
		return
	}
	s.open[key]--
}
