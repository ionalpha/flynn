// Package hlc is a Hybrid Logical Clock: physical wall-clock milliseconds plus a
// logical counter. It orders events consistently across instances even when they
// are offline, reconnect at different times, or have clock skew — which a plain
// per-record version cannot do. Last-writer-wins conflict resolution compares
// (Wall, Counter) then breaks ties by writer id.
//
// The physical clock is injectable, so a replay reproduces the same timestamps
// deterministically; the default is the system clock.
package hlc

import (
	"fmt"
	"math"
	"sync"

	"github.com/ionalpha/flynn/clock"
)

// Time is an HLC timestamp. It is comparable by Wall then Counter; the zero
// value precedes every real timestamp.
type Time struct {
	Wall    int64  // unix milliseconds
	Counter uint16 // logical counter, disambiguating events within the same Wall
}

// IsZero reports whether t is the zero timestamp.
func (t Time) IsZero() bool { return t.Wall == 0 && t.Counter == 0 }

// Compare returns -1, 0, or 1 as t is before, equal to, or after u.
func (t Time) Compare(u Time) int {
	switch {
	case t.Wall != u.Wall:
		if t.Wall < u.Wall {
			return -1
		}
		return 1
	case t.Counter != u.Counter:
		if t.Counter < u.Counter {
			return -1
		}
		return 1
	default:
		return 0
	}
}

// Before reports whether t happened before u.
func (t Time) Before(u Time) bool { return t.Compare(u) < 0 }

// String renders the timestamp as "wall.counter".
func (t Time) String() string { return fmt.Sprintf("%d.%d", t.Wall, t.Counter) }

// Clock issues monotonic HLC timestamps from an injectable physical clock. It is
// safe for concurrent use.
type Clock struct {
	mu   sync.Mutex
	phys clock.Clock
	last Time
}

// Option configures a Clock.
type Option func(*Clock)

// WithPhysical sets the physical clock (default: clock.System).
func WithPhysical(c clock.Clock) Option {
	return func(hc *Clock) {
		if c != nil {
			hc.phys = c
		}
	}
}

// NewClock returns a Clock defaulting to the system physical clock.
func NewClock(opts ...Option) *Clock {
	c := &Clock{phys: clock.System{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Now returns the next timestamp, strictly after every prior Now and Observe.
func (c *Clock) Now() Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	pm := c.phys.Now().UnixMilli()
	if pm > c.last.Wall {
		c.last = Time{Wall: pm}
	} else {
		c.last = tick(c.last)
	}
	return c.last
}

// Observe advances the clock past a timestamp received from another instance
// (during sync), preserving causality, and returns the new local time.
func (c *Clock) Observe(remote Time) Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	pm := c.phys.Now().UnixMilli()
	wall := max3(c.last.Wall, remote.Wall, pm)
	switch {
	case wall == c.last.Wall && wall == remote.Wall:
		c.last = tick(Time{Wall: wall, Counter: maxU16(c.last.Counter, remote.Counter)})
	case wall == c.last.Wall:
		c.last = tick(c.last)
	case wall == remote.Wall:
		c.last = tick(Time{Wall: wall, Counter: remote.Counter})
	default: // physical time is strictly ahead — fresh tick
		c.last = Time{Wall: wall}
	}
	return c.last
}

// tick advances the counter, rolling over to the next millisecond if the counter
// would overflow (a theoretical 65536 events in one millisecond).
func tick(t Time) Time {
	if t.Counter == math.MaxUint16 {
		return Time{Wall: t.Wall + 1}
	}
	return Time{Wall: t.Wall, Counter: t.Counter + 1}
}

func max3(a, b, c int64) int64 { return maxI64(a, maxI64(b, c)) }

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxU16(a, b uint16) uint16 {
	if a > b {
		return a
	}
	return b
}
