package spine

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/clock"
)

// MemoryLog is an in-memory Log: ordered, per-stream monotonic Seq, safe for
// concurrent use. It is the standalone default until the durable (SQLite) log
// lands, and its semantics are the contract that durable implementation must
// match.
type MemoryLog struct {
	clk     clock.Clock
	mu      sync.Mutex
	streams map[string][]Event
}

// MemoryOption configures a MemoryLog.
type MemoryOption func(*MemoryLog)

// WithClock sets the time source used to stamp events whose Time is unset
// (default: clock.System). Tests and replay pass a clock.Manual.
func WithClock(c clock.Clock) MemoryOption { return func(l *MemoryLog) { l.clk = c } }

// NewMemoryLog returns an empty in-memory Log.
func NewMemoryLog(opts ...MemoryOption) *MemoryLog {
	l := &MemoryLog{clk: clock.System{}, streams: map[string][]Event{}}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Append implements Log.
func (l *MemoryLog) Append(_ context.Context, in AppendInput) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := in.Time
	if t.IsZero() {
		t = l.clk.Now()
	}
	e := Event{
		Stream:           in.Stream,
		Seq:              int64(len(l.streams[in.Stream]) + 1),
		Time:             t.UTC(),
		Type:             in.Type,
		Actor:            in.Actor,
		Payload:          clonePayload(in.Payload),
		TraceID:          in.TraceID,
		SpanID:           in.SpanID,
		CausationID:      in.CausationID,
		OriginInstanceID: in.OriginInstanceID,
	}
	l.streams[in.Stream] = append(l.streams[in.Stream], e)
	return e, nil
}

// clonePayload shallow-copies a payload map so the stored event is decoupled
// from the caller's map (the log is immutable). Nested values are shared and
// must be treated as read-only.
func clonePayload(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	c := make(map[string]any, len(p))
	for k, v := range p {
		c[k] = v
	}
	return c
}

// Read implements Log. Returned events share the stored payload maps; callers
// must treat Payload as read-only.
func (l *MemoryLog) Read(_ context.Context, q Query) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	src := l.streams[q.Stream]
	out := make([]Event, 0, len(src))
	for _, e := range src {
		if e.Seq <= q.AfterSeq {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

var _ Log = (*MemoryLog)(nil)
