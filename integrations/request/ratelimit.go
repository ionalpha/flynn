package request

import (
	"encoding/binary"
	"io"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// hostLimiters enforces a minimum interval between requests to the same host. It
// hands out reservation slots spaced interval apart, reading the current time from
// the clock so a clock.Manual makes the spacing deterministic. A zero interval
// disables limiting. Safe for concurrent use.
type hostLimiters struct {
	mu       sync.Mutex
	clk      clock.Clock
	interval time.Duration
	next     map[string]time.Time
}

// reserve claims the next send slot for host and returns how long the caller must
// wait before using it. Slots are spaced interval apart from the later of now and
// the host's previously reserved slot, so concurrent callers queue rather than
// stampede. With a zero interval it always returns 0.
func (h *hostLimiters) reserve(host string) time.Duration {
	if h.interval <= 0 {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clk.Now()
	slot := now
	if prev, ok := h.next[host]; ok && prev.After(now) {
		slot = prev
	}
	h.next[host] = slot.Add(h.interval)
	return slot.Sub(now)
}

// jitter spreads backoff waits using an injected entropy source rather than a
// package-level random generator, so the transport keeps the project's invariant
// that only package ids owns a rand source. A deterministic reader makes the jitter
// reproducible for replay; a nil source means jitter is disabled and backoff stays
// exactly the deterministic schedule. Guarded by a mutex so concurrent retries share
// the source safely.
type jitter struct {
	mu  sync.Mutex
	src io.Reader
}

func newJitter(src io.Reader) *jitter { return &jitter{src: src} }

// upTo returns a value in [0, d) drawn from the entropy source. A non-positive d,
// or any read failure, returns 0, so a starved entropy source degrades to no jitter
// rather than an error.
func (j *jitter) upTo(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var b [8]byte
	if _, err := io.ReadFull(j.src, b[:]); err != nil {
		return 0
	}
	n := int64(binary.BigEndian.Uint64(b[:]) >> 1) // mask the sign bit: always non-negative
	return time.Duration(n % int64(d))
}
