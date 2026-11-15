package request

import (
	"encoding/binary"
	"io"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// hostLimiters spaces requests to one host at least interval apart, reading time
// from the clock so a clock.Manual makes it deterministic. A zero interval disables
// limiting. It is safe for concurrent use.
type hostLimiters struct {
	mu       sync.Mutex
	clk      clock.Clock
	interval time.Duration
	next     map[string]time.Time
}

// reserve claims host's next send slot and returns the wait before using it. Slots
// are spaced interval after the later of now and the previous slot, so concurrent
// callers queue. A zero interval returns 0.
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

// jitter spreads backoff waits from an injected entropy source, because only package
// ids may own a rand source. A deterministic reader makes jitter reproducible for
// replay; a nil source disables it. A mutex lets concurrent retries share the
// source.
type jitter struct {
	mu  sync.Mutex
	src io.Reader
}

func newJitter(src io.Reader) *jitter { return &jitter{src: src} }

// upTo returns a value in [0, d) from the entropy source. A non-positive d or a read
// failure returns 0, so a starved source means no jitter instead of an error.
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
