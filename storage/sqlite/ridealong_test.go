package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// TestRideAlongOverSQLite proves the whole ride-along path works against the durable
// backend with nothing but Flynn and a temp database file: a memory anchored to a
// host's ref surfaces on a read of that ref, the digest's own item is attributed as
// primed while the rest is organic, and both land in usage rows that outlive the
// call. The in-memory tests pin the policy; this pins that the indexed anchor lookup
// and the usage writes behind it really are the same contract on SQL.
func TestRideAlongOverSQLite(t *testing.T) {
	ctx := context.Background()
	p, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ridealong.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	st := p.Memory()

	anchor := state.Anchor{Kind: "ticket", ID: "OPS-17"}
	write := func(content string, anchors ...state.Anchor) state.MemoryItem {
		t.Helper()
		it, err := st.Write(ctx, state.MemoryItem{
			Kind: "fact", Content: content, Sources: []string{"user:operator"}, Anchors: anchors,
		})
		if err != nil {
			t.Fatalf("write %q: %v", content, err)
		}
		return it
	}
	pushed := write("the operator wants the rollback plan stated first", anchor)
	found := write("this deploy needs the release tag", anchor)
	write("nothing to do with the ticket", state.Anchor{Kind: "ticket", ID: "OPS-18"})

	s := ridealong.New(st)
	run := ridealong.NewPrimeScope(ctx)
	if err := s.Push(run, []string{pushed.ID}); err != nil {
		t.Fatalf("push: %v", err)
	}

	got, err := s.Surface(run, state.RecallQuery{Anchors: []state.Anchor{anchor}})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("surfaced %d items, want the 2 anchored to %s", len(got), anchor.ID)
	}

	rows, err := st.Usage(ctx, []string{pushed.ID, found.ID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d, want 2", len(rows))
	}
	byID := map[string]state.MemoryUsage{}
	for _, r := range rows {
		byID[r.MemoryID] = r
	}
	if u := byID[pushed.ID]; u.PushCount != 1 || u.PrimedUses != 1 || u.OrganicUses != 0 {
		t.Errorf("pushed item usage = %+v, want 1 push / 1 primed / 0 organic", u)
	}
	if u := byID[found.ID]; u.PushCount != 0 || u.PrimedUses != 0 || u.OrganicUses != 1 {
		t.Errorf("unpushed item usage = %+v, want 0 pushes / 0 primed / 1 organic", u)
	}
	if u := byID[found.ID]; u.LastUsedAt.IsZero() {
		t.Error("last used at did not persist")
	}
}
