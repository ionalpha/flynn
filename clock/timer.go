package clock

import "time"

// Timer is a single-shot timer, the subset of time.Timer that schedulers need,
// created through a [Timing] clock so a Manual clock decides when it fires.
type Timer interface {
	// C is the channel the timer fires on. It delivers at most one value.
	C() <-chan time.Time
	// Stop prevents a pending timer from firing and reports whether it was still
	// pending, as time.Timer.Stop does.
	Stop() bool
	// Reset restarts the timer to fire d from now and reports whether it was still
	// pending, as time.Timer.Reset does.
	Reset(d time.Duration) bool
}

// Timing is a Clock that can also schedule timers. Code that delays work depends on
// Timing so tests drive every delay with a Manual clock.
type Timing interface {
	Clock
	// NewTimer returns a timer that fires once after d. A d <= 0 fires immediately.
	NewTimer(d time.Duration) Timer
	// After is shorthand for NewTimer(d).C().
	After(d time.Duration) <-chan time.Time
}

type systemTimer struct{ t *time.Timer }

func (s systemTimer) C() <-chan time.Time        { return s.t.C }
func (s systemTimer) Stop() bool                 { return s.t.Stop() }
func (s systemTimer) Reset(d time.Duration) bool { return s.t.Reset(d) }

// NewTimer returns a wall-clock timer.
func (System) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

// After returns a wall-clock channel.
func (System) After(d time.Duration) <-chan time.Time { return time.After(d) }

type manualTimer struct {
	m        *Manual
	deadline time.Time
	c        chan time.Time
}

func (t *manualTimer) C() <-chan time.Time { return t.c }

func (t *manualTimer) Stop() bool {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	return t.m.removeLocked(t)
}

func (t *manualTimer) Reset(d time.Duration) bool {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	was := t.m.removeLocked(t)
	// Drain a stale pending fire so the reset timer starts clean.
	select {
	case <-t.c:
	default:
	}
	t.deadline = t.m.t.Add(d)
	if d <= 0 {
		t.fireLocked()
	} else {
		t.m.timers = append(t.m.timers, t)
	}
	return was
}

// fireLocked delivers the current time without blocking; the channel is buffered
// and fires once. Caller holds mu.
func (t *manualTimer) fireLocked() {
	select {
	case t.c <- t.m.t:
	default:
	}
}

// NewTimer returns a timer that fires when the Manual clock is advanced past d
// from the current time. d <= 0 fires immediately.
func (m *Manual) NewTimer(d time.Duration) Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &manualTimer{m: m, deadline: m.t.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.fireLocked()
	} else {
		m.timers = append(m.timers, t)
	}
	return t
}

// After implements Timing.
func (m *Manual) After(d time.Duration) <-chan time.Time { return m.NewTimer(d).C() }

// removeLocked drops t from the pending set, reporting whether it was present.
// Caller holds mu.
func (m *Manual) removeLocked(t *manualTimer) bool {
	for i, x := range m.timers {
		if x == t {
			m.timers = append(m.timers[:i], m.timers[i+1:]...)
			return true
		}
	}
	return false
}

// fireDueLocked fires every pending timer whose deadline the clock has reached,
// in place. Caller holds mu (Advance/Set call it after moving the clock).
func (m *Manual) fireDueLocked() {
	kept := m.timers[:0]
	for _, t := range m.timers {
		if t.deadline.After(m.t) {
			kept = append(kept, t)
		} else {
			t.fireLocked()
		}
	}
	m.timers = kept
}

var (
	_ Timing = System{}
	_ Timing = (*Manual)(nil)
)
