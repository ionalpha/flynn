package sqlite_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// exercise runs one of every kind of state mutation against p, so a test can
// assert over the resulting event stream and projection.
func exercise(ctx context.Context, t *testing.T, p state.Provider) {
	t.Helper()
	ss, sk, mem := p.Sessions(), p.Skills(), p.Memory()

	s1, err := ss.Create(ctx, state.Session{Title: "first", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := ss.AppendTurn(ctx, state.Turn{SessionID: s1.ID, Role: "user", Content: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := ss.Create(ctx, state.Session{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship", Tags: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sk.Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy", Body: "ship faster"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sk.Upsert(ctx, state.Skill{Slug: "temp", Body: "scratch"}); err != nil {
		t.Fatal(err)
	}
	if err := sk.Delete(ctx, "temp"); err != nil {
		t.Fatal(err)
	}

	m1, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "the user prefers Go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Write(ctx, state.MemoryItem{Kind: "fact", Content: "deploys go to the edge"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.Delete(ctx, m1.ID); err != nil {
		t.Fatal(err)
	}

	// Tombstone a session too, so the stream and projection cover deletes.
	if err := ss.Delete(ctx, s2.ID); err != nil {
		t.Fatal(err)
	}
}

// snapshot serialises the observable state of a provider (live sessions and their
// turns, all live skills, all live memory) to a stable JSON string, so two
// providers can be compared for byte-for-byte equivalence.
func snapshot(ctx context.Context, t *testing.T, p state.Provider) string {
	t.Helper()
	type sessionView struct {
		Session state.Session
		Turns   []state.Turn
	}
	var view struct {
		Sessions []sessionView
		Skills   []state.Skill
		Memory   []state.MemoryItem
	}

	list, err := p.Sessions().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		turns, err := p.Sessions().Turns(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		view.Sessions = append(view.Sessions, sessionView{Session: s, Turns: turns})
	}
	// Search with an empty query returns every live skill regardless of scope.
	if view.Skills, err = p.Skills().Search(ctx, "", 0); err != nil {
		t.Fatal(err)
	}
	if view.Memory, err = p.Memory().Recall(ctx, state.RecallQuery{}); err != nil {
		t.Fatal(err)
	}

	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDurableWritesAreEventSourced is the proof of the command path: every state
// mutation on the SQLite backend appends to the event spine (no write bypasses the
// log), and the state stream alone fully describes the state. It checks both
// halves: the expected events are present, and replaying the SQLite log into a
// fresh in-memory provider reproduces the SQLite reads exactly.
func TestDurableWritesAreEventSourced(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	exercise(ctx, t, store)

	// Every mutation appended to the state stream: the durable backend is
	// event-sourced, not a set of direct table writes.
	events, err := store.Log().Read(ctx, spine.Query{Stream: state.StateStream})
	if err != nil {
		t.Fatal(err)
	}
	// 2 session creates + 2 turns + 3 skill upserts + 1 skill delete +
	// 2 memory writes + 1 memory delete + 1 session delete = 12 events.
	if len(events) != 12 {
		t.Fatalf("state stream has %d events, want 12 (a write bypassed the log?)", len(events))
	}
	want := map[string]bool{
		state.EvSessionCreated: false, state.EvTurnAppended: false, state.EvSessionDeleted: false,
		state.EvSkillUpserted: false, state.EvSkillDeleted: false,
		state.EvMemoryWritten: false, state.EvMemoryDeleted: false,
	}
	for _, e := range events {
		if _, ok := want[e.Type]; !ok {
			t.Fatalf("unexpected state event type %q", e.Type)
		}
		want[e.Type] = true
	}
	for typ, seen := range want {
		if !seen {
			t.Errorf("no %q event on the state stream", typ)
		}
	}

	// The log is authoritative: a fresh in-memory provider folded purely from the
	// SQLite log reproduces the SQLite reads byte for byte.
	replayed, err := state.Replay(ctx, store.Log())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := snapshot(ctx, t, replayed), snapshot(ctx, t, store); got != want {
		t.Fatalf("replayed state differs from SQLite reads\n replay: %s\n sqlite: %s", got, want)
	}
}

// TestRebuildReprojectsFromLog proves the SQLite projection is a derived view: the
// tables can be reprojected from the event log and are unchanged, so the log is the
// source of truth and the tables can always be repaired from it.
func TestRebuildReprojectsFromLog(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	exercise(ctx, t, store)
	before := snapshot(ctx, t, store)

	if err := store.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if after := snapshot(ctx, t, store); after != before {
		t.Fatalf("rebuild changed the projection\n before: %s\n after:  %s", before, after)
	}
	// Rebuild is idempotent: running it again is still a no-op.
	if err := store.Rebuild(ctx); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	if after := snapshot(ctx, t, store); after != before {
		t.Fatalf("second rebuild changed the projection")
	}
}
