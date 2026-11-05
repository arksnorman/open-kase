// Package clock abstracts the current time so time-dependent rules can be
// tested deterministically.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}

// System is the real wall clock. Times are returned in UTC and truncated to
// millisecond precision, which matches MongoDB's date resolution.
type System struct{}

// Now returns the current UTC time.
func (System) Now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// Manual is a controllable clock for tests.
type Manual struct {
	mu  sync.Mutex
	now time.Time
}

// NewManual returns a manual clock starting at t.
func NewManual(t time.Time) *Manual { return &Manual{now: t.UTC()} }

// Now returns the clock's current time.
func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// Advance moves the clock forward by d.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(d)
}

// Set moves the clock to t.
func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = t.UTC()
}
