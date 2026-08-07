package ridealong_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
)

// newStore returns an empty in-memory memory store, closed when the test ends.
func newStore(t *testing.T) state.MemoryStore {
	t.Helper()
	p := state.NewMemory()
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Fatalf("close provider: %v", err)
		}
	})
	return p.Memory()
}

func write(t *testing.T, st state.MemoryStore, content string, anchors ...state.Anchor) state.MemoryItem {
	t.Helper()
	it, err := st.Write(context.Background(), state.MemoryItem{
		Kind:    "fact",
		Content: content,
		Anchors: anchors,
	})
	if err != nil {
		t.Fatalf("write %q: %v", content, err)
	}
	return it
}

// usageOf returns the single usage row for an item, or the zero row when the item
// has never been touched (the store keeps no row for an untouched item).
func usageOf(t *testing.T, st state.MemoryStore, id string) state.MemoryUsage {
	t.Helper()
	rows, err := st.Usage(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(rows) == 0 {
		return state.MemoryUsage{}
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	return rows[0]
}

func ids(items []state.MemoryItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

var taskAnchor = state.Anchor{Kind: "task", ID: "t-1"}

func TestSurfaceReturnsAnchoredItemsAndCountsOrganicUse(t *testing.T) {
	st := newStore(t)
	hit := write(t, st, "deploys need the release tag", taskAnchor)
	other := write(t, st, "unrelated", state.Anchor{Kind: "task", ID: "t-2"})
	loose := write(t, st, "anchored to nothing")

	got, err := ridealong.New(st).Surface(context.Background(), state.RecallQuery{
		Anchors: []state.Anchor{taskAnchor},
	})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if want := []string{hit.ID}; len(got) != 1 || got[0].ID != hit.ID {
		t.Fatalf("surface = %v, want %v", ids(got), want)
	}

	u := usageOf(t, st, hit.ID)
	if u.OrganicUses != 1 || u.PrimedUses != 0 {
		t.Errorf("usage = %d organic / %d primed, want 1/0", u.OrganicUses, u.PrimedUses)
	}
	if u.LastUsedAt.IsZero() {
		t.Error("last used at is zero after a surfacing")
	}
	// Nothing that was not surfaced is counted: a use is a use of what the reader
	// actually got.
	for _, it := range []state.MemoryItem{other, loose} {
		if u := usageOf(t, st, it.ID); u.OrganicUses != 0 || u.PrimedUses != 0 {
			t.Errorf("item %q was counted without being surfaced: %+v", it.Content, u)
		}
	}
}

func TestSurfaceAfterPushCountsPrimedUse(t *testing.T) {
	st := newStore(t)
	pushed := write(t, st, "the operator prefers short answers", taskAnchor)
	found := write(t, st, "the release tag lives in the makefile", taskAnchor)

	s := ridealong.New(st)
	ctx := ridealong.NewPrimeScope(context.Background())
	if err := s.Push(ctx, []string{pushed.ID}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := s.Surface(ctx, state.RecallQuery{Anchors: []state.Anchor{taskAnchor}}); err != nil {
		t.Fatalf("surface: %v", err)
	}

	// The digest's own item is primed: it was in front of the reader already, so
	// this surfacing is not evidence it would have been found.
	if u := usageOf(t, st, pushed.ID); u.PrimedUses != 1 || u.OrganicUses != 0 || u.PushCount != 1 {
		t.Errorf("pushed item usage = %+v, want 1 primed / 0 organic / 1 push", u)
	}
	if u := usageOf(t, st, found.ID); u.OrganicUses != 1 || u.PrimedUses != 0 || u.PushCount != 0 {
		t.Errorf("unpushed item usage = %+v, want 1 organic / 0 primed / 0 pushes", u)
	}
}

func TestPushFailureLeavesTheScopeUnmarked(t *testing.T) {
	st := newStore(t)
	s := ridealong.New(st)
	ctx := ridealong.NewPrimeScope(context.Background())

	// The store checks every id before recording anything, so a push naming an
	// unknown item reaches nobody, and a run that marked it anyway would go on
	// crediting a push that never happened.
	err := s.Push(ctx, []string{"mem-nonexistent"})
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("push of an unknown id = %v, want ErrNotFound", err)
	}
	if got := ridealong.PrimedIDs(ctx); len(got) != 0 {
		t.Errorf("prime scope = %v after a failed push, want empty", got)
	}
}

func TestSurfaceRefusesAQueryWithNoAnchor(t *testing.T) {
	st := newStore(t)
	it := write(t, st, "should not be surfaced by an anchorless query", taskAnchor)
	s := ridealong.New(st)

	cases := map[string]state.RecallQuery{
		"no anchors at all":   {},
		"empty anchor list":   {Anchors: []state.Anchor{}},
		"anchor with no id":   {Anchors: []state.Anchor{{Kind: "task"}}},
		"anchor with no kind": {Anchors: []state.Anchor{{ID: "t-1"}}},
	}
	for name, q := range cases {
		got, err := s.Surface(context.Background(), q)
		if !errors.Is(err, state.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
		if got != nil {
			t.Errorf("%s: returned %v, want nothing", name, ids(got))
		}
	}
	if u := usageOf(t, st, it.ID); u.OrganicUses != 0 {
		t.Errorf("a refused surfacing counted a use: %+v", u)
	}
}

func TestSurfaceCapsResults(t *testing.T) {
	st := newStore(t)
	for range 8 {
		write(t, st, "anchored", taskAnchor)
	}
	q := state.RecallQuery{Anchors: []state.Anchor{taskAnchor}}

	cases := []struct {
		name string
		s    *ridealong.Surfacer
		q    state.RecallQuery
		want int
	}{
		{"default cap", ridealong.New(st), q, 5},
		{"configured cap", ridealong.New(st, ridealong.WithLimit(2)), q, 2},
		// A non-positive limit is not a request for everything, on either side.
		{"cap of zero is the default", ridealong.New(st, ridealong.WithLimit(0)), q, 5},
		{"the query's own limit wins", ridealong.New(st, ridealong.WithLimit(2)), state.RecallQuery{Anchors: q.Anchors, Limit: 7}, 7},
	}
	for _, c := range cases {
		got, err := c.s.Surface(context.Background(), c.q)
		if err != nil {
			t.Fatalf("%s: surface: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: surfaced %d items, want %d", c.name, len(got), c.want)
		}
	}
}

func TestRecallCountsUseAndNeedsNoAnchor(t *testing.T) {
	st := newStore(t)
	hit := write(t, st, "the release tag lives in the makefile")
	miss := write(t, st, "nothing to do with it")

	s := ridealong.New(st)
	got, err := s.Recall(context.Background(), state.RecallQuery{Query: "release tag"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 || got[0].ID != hit.ID {
		t.Fatalf("recall = %v, want [%s]", ids(got), hit.ID)
	}
	if u := usageOf(t, st, hit.ID); u.OrganicUses != 1 {
		t.Errorf("recalled item usage = %+v, want 1 organic use", u)
	}
	if u := usageOf(t, st, miss.ID); u.OrganicUses != 0 {
		t.Errorf("unmatched item was counted: %+v", u)
	}

	// A recall carrying a half-formed anchor is refused like any other, since the
	// filter it would apply is not the one the caller wrote.
	if _, err := s.Recall(context.Background(), state.RecallQuery{Anchors: []state.Anchor{{Kind: "task"}}}); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("recall with a half-formed anchor = %v, want ErrInvalid", err)
	}
}

func TestRecallPrimedByAPushedItem(t *testing.T) {
	st := newStore(t)
	it := write(t, st, "the release tag lives in the makefile")
	s := ridealong.New(st)
	ctx := ridealong.NewPrimeScope(context.Background())
	if err := s.Push(ctx, []string{it.ID}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := s.Recall(ctx, state.RecallQuery{Query: "release tag"}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	// Going and finding an item the digest already showed is still a primed use:
	// the query may well have been prompted by having read it at wake.
	if u := usageOf(t, st, it.ID); u.PrimedUses != 1 || u.OrganicUses != 0 {
		t.Errorf("usage = %+v, want 1 primed / 0 organic", u)
	}
}

// stubStore overrides one method of a real store so a failure can be injected
// without reimplementing the whole interface.
type stubStore struct {
	state.MemoryStore
	recallErr  error
	promotions error
	recordUse  func(ctx context.Context, id string, o state.UsageOrigin) error
}

func (s stubStore) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	if s.recallErr != nil {
		return nil, s.recallErr
	}
	return s.MemoryStore.Recall(ctx, q)
}

func (s stubStore) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	if s.promotions != nil {
		return nil, s.promotions
	}
	return s.MemoryStore.Promotions(ctx, memoryIDs)
}

func (s stubStore) RecordUse(ctx context.Context, id string, o state.UsageOrigin) error {
	if s.recordUse != nil {
		return s.recordUse(ctx, id, o)
	}
	return s.MemoryStore.RecordUse(ctx, id, o)
}

func TestSurfaceReturnsItemsWhenUsageCannotBeCounted(t *testing.T) {
	st := newStore(t)
	it := write(t, st, "still worth reading", taskAnchor)
	boom := errors.New("counter unavailable")
	stub := stubStore{
		MemoryStore: st,
		recordUse:   func(context.Context, string, state.UsageOrigin) error { return boom },
	}

	got, err := ridealong.New(stub).Surface(context.Background(), state.RecallQuery{
		Anchors: []state.Anchor{taskAnchor},
	})
	if !errors.Is(err, ridealong.ErrUsageNotRecorded) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want ErrUsageNotRecorded wrapping the store's error", err)
	}
	// The memory is real and the reader asked for it; only the count was lost.
	if len(got) != 1 || got[0].ID != it.ID {
		t.Errorf("surface = %v, want the item returned alongside the error", ids(got))
	}
}

func TestSurfaceIgnoresAnItemTombstonedMidRead(t *testing.T) {
	st := newStore(t)
	write(t, st, "read then deleted", taskAnchor)
	stub := stubStore{
		MemoryStore: st,
		// The item was really returned and there is nothing left to count it
		// against, so a use that lands on a tombstone is not a failure.
		recordUse: func(context.Context, string, state.UsageOrigin) error { return state.ErrNotFound },
	}

	got, err := ridealong.New(stub).Surface(context.Background(), state.RecallQuery{
		Anchors: []state.Anchor{taskAnchor},
	})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("surfaced %d items, want 1", len(got))
	}
}

func TestReadFailureReturnsNothing(t *testing.T) {
	boom := errors.New("store down")
	stub := stubStore{MemoryStore: newStore(t), recallErr: boom}
	s := ridealong.New(stub)

	if _, err := s.Surface(context.Background(), state.RecallQuery{Anchors: []state.Anchor{taskAnchor}}); !errors.Is(err, boom) {
		t.Errorf("surface err = %v, want the store's error", err)
	}
	if _, err := s.Recall(context.Background(), state.RecallQuery{}); !errors.Is(err, boom) {
		t.Errorf("recall err = %v, want the store's error", err)
	}
}
