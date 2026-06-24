// Package clock is the agent's source of time. Every component reads time
// through a Clock rather than calling time.Now directly, so runs are
// reproducible: tests and deterministic replay supply a Manual clock that only
// advances when told, while production uses System.
//
// This is a deliberate foundational choice - deterministic replay and time-travel
// debugging are impossible if wall-clock time
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
// that only changes via Advance or Set. It is safe for concurrent use. Manual
// also implements Timing: timers it hands out fire only when Advance or Set moves
// the clock past their deadline, so time-based code (the reconciler workqueue)
// stays fully deterministic with no real sleeping.
type Manual struct {
	mu     sync.Mutex
	t      time.Time
	timers []*manualTimer
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

// Advance moves the clock forward by d, firing any timers now due.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = m.t.Add(d)
	m.fireDueLocked()
}

// Set moves the clock to t (normalised to UTC), firing any timers now due.
func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = t.UTC()
	m.fireDueLocked()
}

// PendingTimers returns how many timers are armed and not yet fired. It lets a
// test wait until asynchronously-scheduled work (a queue's backoff timer) has
// registered before advancing the clock, so time-driven behaviour is
// deterministic rather than racy.
func (m *Manual) PendingTimers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.timers)
}

// Compile-time checks that the clock types satisfy Clock.
var (
	_ Clock = System{}
	_ Clock = (*Manual)(nil)
)
