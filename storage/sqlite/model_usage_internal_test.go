package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/usage"
)

func modelUsageRec(run, parent, call string, at time.Time, n int64) usage.Record {
	return usage.Record{
		Run: run, ParentRun: parent, Principal: "alice", Call: call,
		Provider: "anthropic", Model: "claude-opus-5-5", At: at,
		Tokens: usage.Tokens{
			Input:      usage.Count{N: 100 * n, Basis: usage.Reported},
			Output:     usage.Count{N: 10 * n, Basis: usage.Reported},
			CacheRead:  usage.Count{N: 20 * n, Basis: usage.Reported},
			CacheWrite: usage.Count{N: 5 * n, Basis: usage.Reported},
		},
		Cost: usage.Cost{Known: true, Amount: usage.Nanos(1_000_000 * n), Currency: "USD", Basis: usage.Derived},
	}
}

// modelUsageView reads every stored record as JSON, so two states compare byte for byte.
func modelUsageView(t *testing.T, s *Store) string {
	t.Helper()
	got, err := s.Usage().Window(context.Background(), time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func modelUsageEvents(t *testing.T, s *Store) []spine.Event {
	t.Helper()
	all, err := s.Log().Read(context.Background(), spine.Query{Stream: state.StateStream})
	if err != nil {
		t.Fatal(err)
	}
	var out []spine.Event
	for _, e := range all {
		if e.Type == state.EvModelUsageRecorded {
			out = append(out, e)
		}
	}
	return out
}

// TestModelUsageAppendLogsOneEventPerNewBatch: the table is a projection of the log, so
// every stored record has to be on the log, and a retry that stores nothing must
// not add an event that a replay would then have to fold.
func TestModelUsageAppendLogsOneEventPerNewBatch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := modelUsageRec("r1", "", "c1", testAt, 1)
	b := modelUsageRec("r1", "", "c2", testAt.Add(time.Second), 2)

	for _, rs := range [][]usage.Record{{a}, {a}, {a, b}, {a, b}} {
		if err := s.Usage().Append(ctx, rs...); err != nil {
			t.Fatal(err)
		}
	}
	events := modelUsageEvents(t, s)
	if len(events) != 2 {
		t.Fatalf("%d %s events, want 2: one for a, one for the b that was new in the third append", len(events), state.EvModelUsageRecorded)
	}
	first, err := state.DecodeModelUsageRecords(events[0].Payload)
	if err != nil || len(first) != 1 || first[0].Call != "c1" {
		t.Fatalf("first event carries %+v (%v), want call c1", first, err)
	}
	second, err := state.DecodeModelUsageRecords(events[1].Payload)
	if err != nil || len(second) != 1 || second[0].Call != "c2" {
		t.Fatalf("second event carries %+v (%v), want only the new call c2", second, err)
	}

	if err := s.Usage().Append(ctx, modelUsageRec("r1", "", "c1", testAt, 9)); err == nil {
		t.Fatal("a conflicting append succeeded")
	}
	if got := len(modelUsageEvents(t, s)); got != 2 {
		t.Fatalf("a refused append logged an event: %d events", got)
	}

	// A provider folded from the log alone must accept the event.
	if _, err := state.Replay(ctx, s.Log()); err != nil {
		t.Fatalf("Replay of a log with usage events: %v", err)
	}
}

// TestModelUsageRebuildReprojectsFromLog: the table is derived. Emptying it and
// rebuilding from the log restores the same records, because the event carries them
// and the fold needs no clock or price table.
func TestModelUsageRebuildReprojectsFromLog(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for i, rs := range [][]usage.Record{
		{modelUsageRec("root", "", "c1", testAt, 1)},
		{modelUsageRec("kid", "root", "c1", testAt.Add(time.Second), 2), modelUsageRec("kid", "root", "c2", testAt.Add(2*time.Second), 3)},
	} {
		if err := s.Usage().Append(ctx, rs...); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	before := modelUsageView(t, s)

	if _, err := s.db.ExecContext(ctx, `DELETE FROM model_usage`); err != nil {
		t.Fatal(err)
	}
	if got := modelUsageView(t, s); got == before {
		t.Fatal("emptying the table changed nothing; the test proves nothing")
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if after := modelUsageView(t, s); after != before {
		t.Fatalf("rebuild changed the records\n before: %s\n after:  %s", before, after)
	}
	tree, err := s.Usage().ByTree(ctx, "root")
	if err != nil || len(tree) != 3 {
		t.Fatalf("ByTree after rebuild = %d records (%v), want 3", len(tree), err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if after := modelUsageView(t, s); after != before {
		t.Fatal("a second rebuild changed the records")
	}
}

// TestModelUsageSnapshotCarriesRecords: a rebuild resumes from the latest snapshot and
// folds only the events after it, so records stored before the snapshot survive
// only if the snapshot holds them.
func TestModelUsageSnapshotCarriesRecords(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Usage().Append(ctx, modelUsageRec("r1", "", "before", testAt, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotState(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Usage().Append(ctx, modelUsageRec("r1", "", "after", testAt.Add(time.Second), 2)); err != nil {
		t.Fatal(err)
	}
	want := modelUsageView(t, s)

	if _, err := s.db.ExecContext(ctx, `DELETE FROM model_usage`); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if got := modelUsageView(t, s); got != want {
		t.Fatalf("rebuild from a snapshot lost records\n want: %s\n  got: %s", want, got)
	}
}

// TestModelUsageFailsClosed: a usage table that cannot be read or written, or a row
// that cannot be decoded, is an error to the caller and to a rebuild, never an
// empty answer that a budget would read as no spend.
func TestModelUsageFailsClosed(t *testing.T) {
	ctx := context.Background()
	rec := modelUsageRec("r1", "", "c1", testAt, 1)

	t.Run("event with no records", func(t *testing.T) {
		s := newStore(t)
		appendRaw(t, s, state.StateStream, state.EvModelUsageRecorded, `{}`)
		if err := s.Rebuild(ctx); err == nil {
			t.Fatal("rebuild folded a usage event with no records")
		}
	})

	t.Run("event with undecodable records", func(t *testing.T) {
		s := newStore(t)
		appendRaw(t, s, state.StateStream, state.EvModelUsageRecorded, `{"modelUsage":"not a list"}`)
		if err := s.Rebuild(ctx); err == nil {
			t.Fatal("rebuild folded a usage event it could not decode")
		}
	})

	t.Run("table missing", func(t *testing.T) {
		s := newStore(t)
		if err := s.Usage().Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
		dropTables(t, s, "model_usage")
		if err := s.Usage().Append(ctx, modelUsageRec("r2", "", "c1", testAt, 1)); err == nil {
			t.Error("Append succeeded with no table")
		}
		if _, err := s.Usage().ByRun(ctx, "r1"); err == nil {
			t.Error("ByRun succeeded with no table")
		}
		if _, err := s.Usage().ByTree(ctx, "r1"); err == nil {
			t.Error("ByTree succeeded with no table")
		}
		if _, err := s.Usage().Window(ctx, time.Time{}, time.Time{}, ""); err == nil {
			t.Error("Window succeeded with no table")
		}
		if err := s.Rebuild(ctx); err == nil {
			t.Error("rebuild succeeded with no table")
		}
	})

	t.Run("table missing at snapshot", func(t *testing.T) {
		s := newStore(t)
		if err := s.Usage().Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
		dropTables(t, s, "model_usage")
		if err := s.SnapshotState(ctx); err == nil {
			t.Fatal("a snapshot was saved without the usage records")
		}
	})

	t.Run("table missing at restore", func(t *testing.T) {
		s := newStore(t)
		if err := s.Usage().Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if err := s.SnapshotState(ctx); err != nil {
			t.Fatal(err)
		}
		dropTables(t, s, "model_usage")
		if err := s.Rebuild(ctx); err == nil {
			t.Fatal("rebuild from a snapshot succeeded with no usage table")
		}
	})

	t.Run("corrupt row", func(t *testing.T) {
		s := newStore(t)
		if err := s.Usage().Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE model_usage SET record = 'not json'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Usage().ByRun(ctx, "r1"); err == nil {
			t.Error("ByRun returned a row it could not decode")
		}
		if err := s.Usage().Append(ctx, rec); err == nil {
			t.Error("Append compared against a stored row it could not decode")
		}
	})
}

// TestModelUsageQueriesUseTheirIndexes gates the plan of each query: a usage table
// grows with every model call, so a query that scans it pays for all of history.
func TestModelUsageQueriesUseTheirIndexes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	byRun := explainPlan(ctx, t, s, modelUsageByRunSQL, "r1")
	if !strings.Contains(byRun, "USING PRIMARY KEY") {
		t.Errorf("ByRun does not seek the primary key; plan: %s", byRun)
	}

	tree := explainPlan(ctx, t, s, modelUsageTreeSQL, "r1")
	if !strings.Contains(tree, "idx_model_usage_parent") {
		t.Errorf("ByTree does not walk children through idx_model_usage_parent; plan: %s", tree)
	}

	q, args := modelUsageWindowSQL(testAt, testAt.Add(time.Hour), "")
	window := explainPlan(ctx, t, s, q, args...)
	if !strings.Contains(window, "idx_model_usage_at") {
		t.Errorf("Window does not range-scan idx_model_usage_at; plan: %s", window)
	}
	if strings.Contains(window, "TEMP B-TREE") {
		t.Errorf("Window sorts through a temp b-tree instead of reading in index order; plan: %s", window)
	}

	q, args = modelUsageWindowSQL(time.Time{}, time.Time{}, "alice")
	byPrincipal := explainPlan(ctx, t, s, q, args...)
	if !strings.Contains(byPrincipal, "idx_model_usage_principal") {
		t.Errorf("a principal's window does not seek idx_model_usage_principal; plan: %s", byPrincipal)
	}
}

// TestModelUsageSnapshotRefusesAnUndecodableRow: a snapshot that skipped a row it
// could not read would be sealed as complete, and a rebuild that resumed from it
// would total less than the log says with nothing to report why.
func TestModelUsageSnapshotRefusesAnUndecodableRow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Usage().Append(ctx, modelUsageRec("r1", "", "c1", testAt, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE model_usage SET record = 'not json'`); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotState(ctx); err == nil {
		t.Fatal("SnapshotState succeeded over a row that does not decode, want an error")
	}
}
