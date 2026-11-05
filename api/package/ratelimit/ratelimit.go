// Package ratelimit is an in-process token bucket keyed by an arbitrary
// string (principal, client IP).
//
// It is deliberately local: open-kase runs as a single API process (owner
// override O3), so the bound is exact for that deployment and does not depend
// on Redis being up. With several processes each would allow the full rate.
// Memory is bounded by maxKeys; idle buckets (full again) are dropped first,
// and when every bucket is busy the least recently used one is evicted, which
// can only make the limiter more permissive for that key.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter is a set of token buckets sharing one rate.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	maxKeys int
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter allowing perMinute requests per key with bursts of
// burst, tracking at most maxKeys keys. now may be nil (time.Now).
func New(perMinute, burst, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &Limiter{rate: float64(perMinute) / 60, burst: float64(burst), maxKeys: maxKeys, now: now, buckets: map[string]*bucket{}}
}

// Allow takes one token for key. It returns 0 when allowed, otherwise how
// long until a token is available.
func (l *Limiter) Allow(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.evict(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	if l.rate <= 0 {
		return time.Hour
	}
	return time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// Len is the number of tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// evict removes refilled buckets, or the least recently used one.
func (l *Limiter) evict(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
			continue
		}
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = k, b.last
		}
	}
	if len(l.buckets) >= l.maxKeys && oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}
