package ridealong_test

import (
	"context"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
)

// sourceClasses spans the provenance classes admission splits on, including the
// item that records no provenance at all.
var sourceClasses = [][]string{
	{"user:operator"},
	{"agent:distiller"},
	{"run-42"},
	nil,
	{"tool:shell"},
	{"web:example.com"},
	{"inbound:signal"},
	{"user:operator", "web:example.com"},
}

// corpus writes a drawn set of anchored items to a fresh store and returns it.
func corpus(rt *rapid.T, st state.MemoryStore) []state.MemoryItem {
	n := rapid.IntRange(1, 8).Draw(rt, "items")
	out := make([]state.MemoryItem, 0, n)
	for i := range n {
		it, err := st.Write(context.Background(), state.MemoryItem{
			Kind:    "fact",
			Content: rapid.StringMatching(`[a-z ]{1,24}`).Draw(rt, "content"),
			Sources: rapid.SampledFrom(sourceClasses).Draw(rt, "sources"),
			Tainted: rapid.Bool().Draw(rt, "tainted"),
			Anchors: []state.Anchor{taskAnchor},
		})
		if err != nil {
			rt.Fatalf("write item %d: %v", i, err)
		}
		out = append(out, it)
	}
	return out
}

func freshStore(rt *rapid.T) state.MemoryStore {
	p := state.NewMemory()
	rt.Cleanup(func() { _ = p.Close() })
	return p.Memory()
}

func surfaceAll(ctx context.Context, rt *rapid.T, s *ridealong.Surfacer) []string {
	got, err := s.Surface(ctx, state.RecallQuery{Anchors: []state.Anchor{taskAnchor}, Limit: 100})
	if err != nil {
		rt.Fatalf("surface: %v", err)
	}
	out := ids(got)
	slices.Sort(out)
	return out
}

// TestProp_AdmissionNarrowsMonotonically: whatever the corpus, the three policies
// nest. Pushable admits no more than the default, the default admits no more than
// everything, and nothing tainted or untrusted-origin ever reaches a reader who did
// not ask under either gated policy. This is the invariant the gate exists for, and
// the one a future selection change must not break.
func TestProp_AdmissionNarrowsMonotonically(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		st := freshStore(rt)
		items := corpus(rt, st)
		byID := map[string]state.MemoryItem{}
		for _, it := range items {
			byID[it.ID] = it
		}

		all := surfaceAll(ctx, rt, ridealong.New(st, ridealong.WithAdmission(ridealong.AdmitAll)))
		undenied := surfaceAll(ctx, rt, ridealong.New(st))
		pushable := surfaceAll(ctx, rt, ridealong.New(st, ridealong.WithAdmission(ridealong.AdmitPushable)))

		if len(all) != len(items) {
			rt.Fatalf("AdmitAll surfaced %d of %d anchored items", len(all), len(items))
		}
		for _, id := range pushable {
			if !slices.Contains(undenied, id) {
				rt.Fatalf("item %s is pushable but not admitted by the default policy", id)
			}
		}
		for _, id := range undenied {
			if !slices.Contains(all, id) {
				rt.Fatalf("item %s is admitted by the default policy but not by AdmitAll", id)
			}
			if it := byID[id]; it.Tainted {
				rt.Fatalf("tainted item %s rode along", id)
			}
		}
		// With nothing promoted, the strict policy is exactly the operator's own
		// untainted memory.
		for _, id := range pushable {
			it := byID[id]
			if it.Tainted || !slices.Equal(it.Sources, []string{"user:operator"}) {
				rt.Fatalf("item %s cleared the strict gate with sources %v tainted=%v", id, it.Sources, it.Tainted)
			}
		}
	})
}

// TestProp_SurfacingCountsExactlyWhatItReturned: a surfacing counts one use of each
// item it handed over and of nothing else, and every one of those uses is primed
// exactly when the run had already been shown that item. The decay policy reads
// these counters, so an over-count or an under-count is a measurement that lies.
func TestProp_SurfacingCountsExactlyWhatItReturned(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st := freshStore(rt)
		items := corpus(rt, st)
		s := ridealong.New(st, ridealong.WithAdmission(ridealong.AdmitAll))
		ctx := ridealong.NewPrimeScope(context.Background())

		pushedIdx := rapid.SliceOfDistinct(rapid.IntRange(0, len(items)-1), func(i int) int { return i }).Draw(rt, "pushed")
		pushed := make([]string, 0, len(pushedIdx))
		for _, i := range pushedIdx {
			pushed = append(pushed, items[i].ID)
		}
		if err := s.Push(ctx, pushed); err != nil {
			rt.Fatalf("push: %v", err)
		}

		returned := surfaceAll(ctx, rt, s)
		rows, err := st.Usage(context.Background(), nil)
		if err != nil {
			rt.Fatalf("usage: %v", err)
		}
		for _, r := range rows {
			wantUses := int64(0)
			if slices.Contains(returned, r.MemoryID) {
				wantUses = 1
			}
			primed := int64(0)
			if wantUses == 1 && slices.Contains(pushed, r.MemoryID) {
				primed = 1
			}
			if r.OrganicUses+r.PrimedUses != wantUses {
				rt.Fatalf("item %s counted %d uses, want %d", r.MemoryID, r.OrganicUses+r.PrimedUses, wantUses)
			}
			if r.PrimedUses != primed {
				rt.Fatalf("item %s counted %d primed uses, want %d", r.MemoryID, r.PrimedUses, primed)
			}
		}
	})
}

// TestProp_PrimeScopeIsExactlyWhatWasMarked: the scope answers primed for the ids
// marked on it and organic for everything else, whatever order and however many
// times they were marked.
func TestProp_PrimeScopeIsExactlyWhatWasMarked(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := ridealong.NewPrimeScope(context.Background())
		marked := map[string]bool{}
		batches := rapid.IntRange(1, 4).Draw(rt, "batches")
		for range batches {
			batch := rapid.SliceOf(rapid.StringMatching(`[a-z0-9-]{0,6}`)).Draw(rt, "batch")
			ridealong.MarkPushed(ctx, batch...)
			for _, id := range batch {
				if id != "" {
					marked[id] = true
				}
			}
		}

		for id := range marked {
			if ridealong.OriginFor(ctx, id) != state.UsagePrimed {
				rt.Fatalf("marked id %q reports organic", id)
			}
		}
		got := ridealong.PrimedIDs(ctx)
		if !slices.IsSorted(got) || len(got) != len(marked) {
			rt.Fatalf("PrimedIDs = %v, want the %d marked ids sorted", got, len(marked))
		}
		unmarked := rapid.StringMatching(`[A-Z]{1,6}`).Draw(rt, "unmarked")
		if ridealong.OriginFor(ctx, unmarked) != state.UsageOrganic {
			rt.Fatalf("unmarked id %q reports primed", unmarked)
		}
	})
}

// TestProp_SurfacingNeverExceedsItsCap: whatever the corpus and whichever cap is in
// force, a ride-along returns no more than the cap. It rides on somebody else's
// response, so an unbounded one is a context leak with a memory label on it.
func TestProp_SurfacingNeverExceedsItsCap(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st := freshStore(rt)
		corpus(rt, st)
		limit := rapid.IntRange(1, 10).Draw(rt, "cap")
		queryLimit := rapid.IntRange(0, 10).Draw(rt, "queryLimit")

		got, err := ridealong.New(st, ridealong.WithLimit(limit), ridealong.WithAdmission(ridealong.AdmitAll)).
			Surface(context.Background(), state.RecallQuery{Anchors: []state.Anchor{taskAnchor}, Limit: queryLimit})
		if err != nil {
			rt.Fatalf("surface: %v", err)
		}
		want := queryLimit
		if want <= 0 {
			want = limit
		}
		if len(got) > want {
			rt.Fatalf("surfaced %d items against a cap of %d", len(got), want)
		}
	})
}
