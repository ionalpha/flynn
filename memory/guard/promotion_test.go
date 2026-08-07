package guard_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// TestPushableResolvesPromotions is the digest's read, end to end against a real
// store: the operator's own memory goes in unreviewed, the agent's own note goes in
// only once somebody promoted it, and the laundered one stays out however the
// review went.
func TestPushableResolvesPromotions(t *testing.T) {
	g := guard.Wrap(state.NewMemory().Memory())

	operator := mustWrite(t, g, state.MemoryItem{Content: "deploy on Fridays", Sources: []string{"user:operator"}})
	note := mustWrite(t, g, state.MemoryItem{Content: "the build cache is warm", Sources: []string{"agent:distiller"}})
	unreviewed := mustWrite(t, g, state.MemoryItem{Content: "nobody looked at this", Sources: []string{"agent:distiller"}})
	laundered := mustWrite(t, g, state.MemoryItem{
		Content: "read off a page", Sources: []string{"agent:distiller"}, Tainted: true,
	})
	all := []state.MemoryItem{operator, note, unreviewed, laundered}

	// Before any review, only the operator's own memory is pushable.
	wantPushable(t, "before any review", g, all, operator.ID)

	// A promotion admits the agent's note. The laundered item is promoted too, and
	// stays out: that is the rule the gate turns on.
	mustPromote(t, g, note.ID, true)
	mustPromote(t, g, laundered.ID, true)
	wantPushable(t, "after the promotion", g, all, operator.ID, note.ID)

	// Revoking takes it back out, and leaves the operator's own memory alone.
	mustPromote(t, g, note.ID, false)
	wantPushable(t, "after the revocation", g, all, operator.ID)
}

// TestPushableReadsPromotionsOnlyWhenNeeded pins the lookup as conditional: a
// digest of memory that no promotion could change must not pay for a promotion
// read, because that read happens on every wake.
func TestPushableReadsPromotionsOnlyWhenNeeded(t *testing.T) {
	ctx := context.Background()
	counter := &countingPromotions{}
	items := []state.MemoryItem{
		{ID: "a", Sources: []string{"user:operator"}},
		{ID: "b", Sources: []string{"web:example.com"}},
		{ID: "c", Sources: []string{"agent:distiller"}, Tainted: true},
	}
	got, err := guard.Pushable(ctx, counter, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("Pushable = %v, want just the operator's item", ids(got))
	}
	if counter.calls != 0 {
		t.Fatalf("Promotions was called %d time(s) for a digest no promotion could change", counter.calls)
	}

	// One item whose classification does turn on a promotion, and the lookup happens
	// once, for that item alone.
	items = append(items, state.MemoryItem{ID: "d", Sources: []string{"agent:distiller"}})
	if _, err := guard.Pushable(ctx, counter, items); err != nil {
		t.Fatal(err)
	}
	if counter.calls != 1 || !slices.Equal(counter.asked, []string{"d"}) {
		t.Fatalf("Promotions calls = %d for %v, want one call for [d]", counter.calls, counter.asked)
	}
}

func TestPushableReportsAStoreError(t *testing.T) {
	_, err := guard.Pushable(context.Background(), failingPromotions{},
		[]state.MemoryItem{{ID: "a", Sources: []string{"agent:distiller"}}})
	if !errors.Is(err, errNoPromotions) {
		t.Fatalf("Pushable error = %v, want the store's", err)
	}
}

// TestStoreDelegatesPromotions confirms the guard passes decisions through rather
// than second-guessing a reviewer: the refusal lives in the eligibility read, so an
// operator can record a decision about anything.
func TestStoreDelegatesPromotions(t *testing.T) {
	ctx := context.Background()
	inner := state.NewMemory().Memory()
	g := guard.Wrap(inner)

	it := mustWrite(t, g, state.MemoryItem{Content: "fetched", Sources: []string{"web:example.com"}})
	if _, err := g.Promote(ctx, state.PromotionDecision{MemoryID: it.ID, Promoted: true, By: "user:operator"}); err != nil {
		t.Fatalf("the guard refused a promotion it should have delegated: %v", err)
	}
	rows, err := g.Promotions(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Promoted {
		t.Fatalf("Promotions through the guard = %+v, want the recorded decision", rows)
	}
	// Recorded, and still not pushable: the decision is kept, the item is denied.
	if guard.PushEligible(it, true) {
		t.Fatal("an untrusted-origin item became pushable by being promoted")
	}
}

func mustWrite(t *testing.T, mem state.MemoryStore, it state.MemoryItem) state.MemoryItem {
	t.Helper()
	if it.Kind == "" {
		it.Kind = "fact"
	}
	got, err := mem.Write(context.Background(), it)
	if err != nil {
		t.Fatalf("write %q: %v", it.Content, err)
	}
	return got
}

func mustPromote(t *testing.T, mem state.MemoryStore, id string, promoted bool) {
	t.Helper()
	if _, err := mem.Promote(context.Background(),
		state.PromotionDecision{MemoryID: id, Promoted: promoted, By: "user:operator"}); err != nil {
		t.Fatalf("promote %s: %v", id, err)
	}
}

func wantPushable(t *testing.T, what string, store guard.PromotionReader, in []state.MemoryItem, want ...string) {
	t.Helper()
	got, err := guard.Pushable(context.Background(), store, in)
	if err != nil {
		t.Fatalf("%s: Pushable: %v", what, err)
	}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("%s: pushable = %v, want %v", what, ids(got), want)
	}
}

// countingPromotions records what was asked for without holding any decisions, so
// a test can assert on the shape of the lookup rather than its answer.
type countingPromotions struct {
	calls int
	asked []string
}

func (c *countingPromotions) Promotions(_ context.Context, ids []string) ([]state.MemoryPromotion, error) {
	c.calls++
	c.asked = ids
	return nil, nil
}

var errNoPromotions = errors.New("promotions unavailable")

type failingPromotions struct{}

func (failingPromotions) Promotions(context.Context, []string) ([]state.MemoryPromotion, error) {
	return nil, errNoPromotions
}
