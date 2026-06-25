package spine

import (
	"context"
	"sort"
	"sync"

	"github.com/ionalpha/flynn/clock"
)

// MemoryLog is an in-memory Log and SnapshotStore: ordered, per-stream monotonic
// Seq, safe for concurrent use. Its semantics are the contract the durable
// (SQLite) implementation must match.
type MemoryLog struct {
	clk       clock.Clock
	mu        sync.Mutex
	streams   map[string][]Event
	snapshots map[string][]Snapshot
}

// MemoryOption configures a MemoryLog.
type MemoryOption func(*MemoryLog)

// WithClock sets the time source used to stamp events whose Time is unset
// (default: clock.System). Tests and replay pass a clock.Manual.
func WithClock(c clock.Clock) MemoryOption { return func(l *MemoryLog) { l.clk = c } }

// NewMemoryLog returns an empty in-memory Log.
func NewMemoryLog(opts ...MemoryOption) *MemoryLog {
	l := &MemoryLog{clk: clock.System{}, streams: map[string][]Event{}, snapshots: map[string][]Snapshot{}}
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
	version := in.SchemaVersion
	if version == 0 {
		version = DefaultSchemaVersion
	}
	e := Event{
		Stream:           in.Stream,
		Seq:              int64(len(l.streams[in.Stream]) + 1),
		Time:             t.UTC(),
		Type:             in.Type,
		Actor:            in.Actor,
		Payload:          clonePayload(in.Payload),
		SchemaVersion:    version,
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

// SaveSnapshot implements SnapshotStore. It clones the payload so the stored
// snapshot is decoupled from the caller's slice, and replaces any snapshot already
// at (Stream, Seq).
func (l *MemoryLog) SaveSnapshot(_ context.Context, s Snapshot) error {
	s.Payload = append([]byte(nil), s.Payload...)
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.snapshots[s.Stream]
	for i := range list {
		if list[i].Seq == s.Seq {
			list[i] = s
			return nil
		}
	}
	list = append(list, s)
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	l.snapshots[s.Stream] = list
	return nil
}

// LatestSnapshot implements SnapshotStore. Returned payloads are cloned, so a
// caller may retain or mutate them freely.
func (l *MemoryLog) LatestSnapshot(_ context.Context, stream string, upToSeq int64) (Snapshot, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var best Snapshot
	found := false
	for _, s := range l.snapshots[stream] {
		if upToSeq > 0 && s.Seq > upToSeq {
			continue
		}
		if !found || s.Seq > best.Seq {
			best, found = s, true
		}
	}
	if found {
		best.Payload = append([]byte(nil), best.Payload...)
	}
	return best, found, nil
}

var _ Log = (*MemoryLog)(nil)
