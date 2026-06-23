package sqlite_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/spine/sqlite"
)

// logModel drives the durable Log through random Append/Read sequences and
// checks the ordering invariants after every step — the deterministic-simulation
// tier, run against real SQLite (transactions, JSON payload round-trips, the
// per-stream MAX(seq) assignment) rather than the in-memory map.
type logModel struct {
	ctx    context.Context
	log    *sqlite.Log
	stream string
	count  int
}

func (m *logModel) Append(t *rapid.T) {
	e, err := m.log.Append(m.ctx, testkit.AppendInputGen(m.stream).Draw(t, "in"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	m.count++
	if e.Seq != int64(m.count) {
		t.Fatalf("append assigned Seq %d, want %d", e.Seq, m.count)
	}
}

func (m *logModel) ReadAll(t *rapid.T) {
	got, err := m.log.Read(m.ctx, spine.Query{Stream: m.stream})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != m.count {
		t.Fatalf("read %d events, model has %d", len(got), m.count)
	}
}

func (m *logModel) ReadAfter(t *rapid.T) {
	after := rapid.IntRange(0, m.count).Draw(t, "after")
	got, err := m.log.Read(m.ctx, spine.Query{Stream: m.stream, AfterSeq: int64(after)})
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if len(got) != m.count-after {
		t.Fatalf("read after %d: got %d, want %d", after, len(got), m.count-after)
	}
	if len(got) > 0 && got[0].Seq != int64(after+1) {
		t.Fatalf("read after %d: first Seq %d, want %d", after, got[0].Seq, after+1)
	}
}

func (m *logModel) ReadWithLimit(t *rapid.T) {
	limit := rapid.IntRange(1, 5).Draw(t, "limit")
	got, err := m.log.Read(m.ctx, spine.Query{Stream: m.stream, Limit: limit})
	if err != nil {
		t.Fatalf("read limit: %v", err)
	}
	want := min(m.count, limit)
	if len(got) != want {
		t.Fatalf("read limit %d: got %d, want %d", limit, len(got), want)
	}
}

// Check runs after every action: the stream is always a dense, ordered 1..N.
func (m *logModel) Check(t *rapid.T) {
	got, err := m.log.Read(m.ctx, spine.Query{Stream: m.stream})
	if err != nil {
		t.Fatalf("check read: %v", err)
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("invariant: event[%d] Seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Stream != m.stream {
			t.Fatalf("invariant: event on foreign stream %q", e.Stream)
		}
	}
}

func TestSQLiteLogStateMachine(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		l, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = l.Close() }()
		m := &logModel{ctx: context.Background(), log: l, stream: "run"}
		t.Repeat(rapid.StateMachineActions(m))
	})
}
