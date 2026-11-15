// Package clock is the agent's source of time. Components read a [Clock] instead of
// calling time.Now, so tests and deterministic replay can supply a [Manual] clock
// that advances only when told; production uses [System].
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

// Manual is a deterministic Clock for tests and replay: its time changes only on
// Advance or Set, and the timers it hands out fire only then, so timer-driven code
// such as the reconciler workqueue needs no real sleeping. It is safe for
// concurrent use.
type Manual struct {
	mu     sync.Mutex
	t      time.Time
	timers []*manualTimer
}

// NewManual returns a Manual clock started at start, in UTC.
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

// Set moves the clock to t, in UTC, firing any timers now due.
func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = t.UTC()
	m.fireDueLocked()
}

// PendingTimers returns how many timers are armed and unfired, so a test can wait
// for asynchronously scheduled work to register its timer before advancing.
func (m *Manual) PendingTimers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.timers)
}

var (
	_ Clock = System{}
	_ Clock = (*Manual)(nil)
)
