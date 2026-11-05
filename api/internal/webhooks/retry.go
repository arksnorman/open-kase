package webhooks

import (
	"math/rand/v2"
	"time"
)

// Schedule is the default delay before each retry after a failed attempt:
// the first retry follows 1 minute after the first failure, and so on. After
// the last entry is used, the next failure dead-letters the delivery for
// manual retry. Total attempts: 1 + len(Schedule).
var Schedule = []time.Duration{
	time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	12 * time.Hour,
}

// JitterFraction spreads retries by ±20% to avoid synchronized bursts.
const JitterFraction = 0.2

// Jitter returns a delay near base. DefaultJitter is used in production;
// tests pass a deterministic function.
type Jitter func(base time.Duration) time.Duration

// DefaultJitter applies a uniform ±JitterFraction jitter.
func DefaultJitter(base time.Duration) time.Duration {
	f := 1 + JitterFraction*(2*rand.Float64()-1)
	return time.Duration(float64(base) * f)
}

// Decision is what to do after an attempt.
type Decision struct {
	Done       bool          // delivered; nothing more to do
	DeadLetter bool          // stop retrying; keep for manual retry
	Delay      time.Duration // wait before the next attempt when retrying
}

// Decide returns the next step after attempt number attempt (1-based) ended
// with res. A receiver's Retry-After (already capped by the client policy)
// can lengthen, never shorten, the scheduled delay.
func Decide(attempt int, res Result, jitter Jitter) Decision {
	switch res.Outcome {
	case OutcomeDelivered:
		return Decision{Done: true}
	case OutcomePermanent:
		return Decision{DeadLetter: true}
	}
	if attempt < 1 || attempt > len(Schedule) {
		return Decision{DeadLetter: true}
	}
	if jitter == nil {
		jitter = DefaultJitter
	}
	delay := jitter(Schedule[attempt-1])
	if res.RetryAfter > delay {
		delay = res.RetryAfter
	}
	return Decision{Delay: delay}
}
