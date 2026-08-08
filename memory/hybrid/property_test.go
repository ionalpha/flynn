package hybrid_test

import (
	"context"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory/hybrid"
	"github.com/ionalpha/flynn/state"
)

// drawPhrases are the sentences a drawn corpus is built from. They overlap in
// concept and differ in wording, which is the situation the fusion is for.
var drawPhrases = []string{
	"we settled on Postgres for the primary datastore",
	"the storage engine decision is ours to revisit",
	"the release ships Thursday afternoon",
	"the rollout window moved",
	"the user prefers tabs",
	"indentation style is a preference not a rule",
	"the suite is flaky on timeout",
	"ticket OPS-4821 is still open",
}

var drawQueries = []string{
	"which storage engine did we choose",
	"when does it ship",
	"OPS-4821",
	"what does the user like",
	"why is the suite failing",
}

// TestRecallInvariants holds the fused ranking to its contract over drawn
// corpora and queries: the scale scores are reported on, the order they come
// back in, the selectors that bound them, and the one guarantee fusion owes the
// measure it is added to.
func TestRecallInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		phrases := rapid.SliceOfN(rapid.SampledFrom(drawPhrases), 1, 8).Draw(rt, "corpus")
		query := rapid.SampledFrom(drawQueries).Draw(rt, "query")
		limit := rapid.IntRange(0, 4).Draw(rt, "limit")
		floor := rapid.Float64Range(0, 1).Draw(rt, "floor")

		clk := clock.NewManual(epoch)
		p := state.NewMemory(state.WithClock(clk))
		defer func() {
			if err := p.Close(); err != nil {
				rt.Fatalf("close provider: %v", err)
			}
		}()
		inner := p.Memory()
		for i, phrase := range phrases {
			if _, err := inner.Write(context.Background(), state.MemoryItem{Kind: "note", Content: phrase}); err != nil {
				rt.Fatalf("write %d: %v", i, err)
			}
			clk.Advance(time.Minute)
		}
		store := hybrid.Wrap(inner, hybrid.WithEmbedder(&fakeEmbedder{}))

		q := state.RecallQuery{Query: query, Order: state.OrderRelevance, Limit: limit, MinScore: floor}
		got, err := store.Recall(context.Background(), q)
		if err != nil {
			rt.Fatalf("recall: %v", err)
		}
		if limit > 0 && len(got) > limit {
			rt.Fatalf("recall returned %d items, over the limit of %d", len(got), limit)
		}
		for i, it := range got {
			if it.Score <= 0 || it.Score > 1 {
				rt.Fatalf("score[%d] = %v, want it in (0,1]", i, it.Score)
			}
			if it.Score < floor {
				rt.Fatalf("score[%d] = %v, under the floor of %v", i, it.Score, floor)
			}
			if i > 0 && got[i-1].Score < it.Score {
				rt.Fatalf("scores out of relevance order at %d: %v then %v", i, got[i-1].Score, it.Score)
			}
		}

		// The guarantee fusion owes the measure it was added to: an unbounded fused
		// recall never loses a word match. Adding a second opinion may reorder the
		// results and may add to them, and it may never drop what the store found on
		// its own.
		unbounded, err := store.Recall(context.Background(), state.RecallQuery{Query: query, Order: state.OrderRelevance})
		if err != nil {
			rt.Fatalf("unbounded recall: %v", err)
		}
		fused := make(map[string]bool, len(unbounded))
		for _, it := range unbounded {
			fused[it.ID] = true
		}
		lexical, err := inner.Recall(context.Background(), state.RecallQuery{Query: query})
		if err != nil {
			rt.Fatalf("lexical recall: %v", err)
		}
		for _, it := range lexical {
			if !fused[it.ID] {
				rt.Fatalf("fused recall dropped the lexical hit %q", it.Content)
			}
		}
	})
}
