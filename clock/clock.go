// Package clock is the agent's source of time. Every component reads time
// through a Clock rather than calling time.Now directly, so runs are
// reproducible: tests and deterministic replay supply a Manual clock that only
// advances when told, while production uses System.
//
// This is a deliberate substrate choice — the README promises deterministic
// replay and time-travel debugging, which is impossible if wall-clock time
// leaks in at arbitrary call sites.
package clock

import (
	"sync"
	"time"
)

// Clock is the agent's source of the current time.
type Clock interface {
	// Now returns the current time. Implementations should return UTC.
	Now() time.Time
}

// System is the production Clock, backed by the wall clock in UTC.
type System struct{}

// Now implements Clock.
func (System) Now() time.Time { return time.Now().UTC() }

// Manual is a deterministic Clock for tests and replay: it returns a fixed time
// that only changes via Advance or Set. It is safe for concurrent use.
type Manual struct {
	mu sync.Mutex
	t  time.Time
}

// NewManual returns a Manual clock started at start (normalised to UTC).
func NewManual(start time.Time) *Manual {
	return &Manual{t: start.UTC()}
}

// Now implements Clock.
func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t
}

// Advance moves the clock forward by d.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = m.t.Add(d)
}

// Set moves the clock to t (normalised to UTC).
func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = t.UTC()
}

// Compile-time checks that the clock types satisfy Clock.
var (
	_ Clock = System{}
	_ Clock = (*Manual)(nil)
)
