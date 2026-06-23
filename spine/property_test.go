package spine_test

import (
	"context"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/spinesink"
	"github.com/ionalpha/flynn/state"
)

// --- Model-based (deterministic-simulation) testing of the log --------------

// logModel drives a MemoryLog through random Append/Read sequences and checks
// the ordering invariants after every step. rapid explores long randomized
// histories and shrinks any failure to a minimal reproducer — this is the DST
// tier on the event spine, in a few dozen lines.
type logModel struct {
	ctx    context.Context
	log    *spine.MemoryLog
	stream string
	count  int // model: how many events have been appended
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

func TestSpineLogStateMachine(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := &logModel{ctx: context.Background(), log: spine.NewMemoryLog(), stream: "run"}
		t.Repeat(rapid.StateMachineActions(m))
	})
}

// --- Sink branch coverage (scope + classified error in the payload) ---------

func TestSinkRecordsScopeAndErrorClass(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	d := dispatch.New(
		dispatch.HandlerFunc(func(context.Context, dispatch.Action) (dispatch.Result, error) {
			return dispatch.Result{}, fault.New(fault.Transient, "boom", "x")
		}),
		dispatch.WithEventSink(spinesink.New(log, "run")),
	)
	if _, err := d.Dispatch(ctx, dispatch.Action{Name: "fetch", Scope: state.Scope{Project: "alpha"}}); err == nil {
		t.Fatal("expected the handler error to propagate")
	}

	events, err := log.Read(ctx, spine.Query{Stream: "run"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var end spine.Event
	for _, e := range events {
		if e.Type == dispatch.EventEnd {
			end = e
		}
	}
	if end.Payload["error_class"] != string(fault.Transient) {
		t.Fatalf("end payload error_class = %v, want transient", end.Payload["error_class"])
	}
	scope, ok := end.Payload["scope"].(map[string]string)
	if !ok || scope["project"] != "alpha" {
		t.Fatalf("end payload scope = %v, want project=alpha", end.Payload["scope"])
	}
}

// --- Golden snapshot of a deterministic replay (dogfoods testkit.Golden) -----

func TestSpineReplayGolden(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog(spine.WithClock(clock.NewManual(time.Unix(1_700_000_000, 0))))
	for _, typ := range []string{"start", "tool", "end"} {
		if _, err := log.Append(ctx, spine.AppendInput{Stream: "run", Type: typ, Actor: spine.ActorAgent}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	events, err := log.Read(ctx, spine.Query{Stream: "run"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// A whole replay as one snapshot — no hand-written expected value.
	testkit.Golden(t, "spine_replay", events)
}
