package digest_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory/digest"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// sourceClasses spans the provenance classes the push gate splits on, including
// the item that records no provenance at all.
var sourceClasses = [][]string{
	{"user:operator"},
	{"agent:distiller"},
	{"run-42"},
	nil,
	{"tool:shell"},
	{"web:example.com"},
	{"user:operator", "web:example.com"},
}

// scopeClasses spans the ancestor chain of the workspace scope the properties
// build a digest at, so a drawn corpus straddles several levels of it.
var scopeClasses = []state.Scope{
	{Instance: "i", Project: "p", Workspace: "w"},
	{Instance: "i", Project: "p"},
	{Instance: "i"},
	{},
	{Instance: "i", Project: "other"},
}

var propScope = state.Scope{Instance: "i", Project: "p", Workspace: "w"}

// propCorpus writes a drawn set of items to a fresh store, promoting and pushing
// some of them, and returns the store alongside what was written.
func propCorpus(rt *rapid.T) (state.MemoryStore, []state.MemoryItem) {
	clk := clock.NewManual(epoch)
	p := state.NewMemory(state.WithClock(clk))
	rt.Cleanup(func() { _ = p.Close() })
	st := p.Memory()

	n := rapid.IntRange(0, 12).Draw(rt, "items")
	out := make([]state.MemoryItem, 0, n)
	for i := range n {
		it, err := st.Write(context.Background(), state.MemoryItem{
			Kind:    rapid.SampledFrom([]string{"fact", "preference", "lesson"}).Draw(rt, "kind"),
			Content: rapid.StringMatching(`[a-z][a-z ]{0,80}`).Draw(rt, "content"),
			Sources: rapid.SampledFrom(sourceClasses).Draw(rt, "sources"),
			Tainted: rapid.Bool().Draw(rt, "tainted"),
			Scope:   rapid.SampledFrom(scopeClasses).Draw(rt, "scope"),
		})
		if err != nil {
			rt.Fatalf("write item %d: %v", i, err)
		}
		clk.Advance(time.Minute)
		if rapid.Bool().Draw(rt, "promoted") {
			if _, err := st.Promote(context.Background(), state.PromotionDecision{
				MemoryID: it.ID, Promoted: true, By: "op",
			}); err != nil {
				rt.Fatalf("promote: %v", err)
			}
		}
		// The push range crosses the demotion threshold in both directions, and the
		// use is what decides which side of it an often-pushed item lands on, so every
		// property below is drawn against corpora with and without demoted items in
		// them.
		for range rapid.IntRange(0, 6).Draw(rt, "prior pushes") {
			if err := st.RecordPush(context.Background(), []string{it.ID}); err != nil {
				rt.Fatalf("record push: %v", err)
			}
		}
		if rapid.Bool().Draw(rt, "used") {
			if err := st.RecordUse(context.Background(), it.ID, state.UsagePrimed); err != nil {
				rt.Fatalf("record use: %v", err)
			}
		}
		out = append(out, it)
	}
	return st, out
}

func propBuilder(rt *rapid.T, st state.MemoryStore) *digest.Builder {
	return digest.New(st,
		digest.WithBudget(rapid.IntRange(1, 400).Draw(rt, "budget")),
		digest.WithExplorationQuota(rapid.Float64Range(0, 1).Draw(rt, "quota")),
		digest.WithSummaryChars(rapid.IntRange(4, 200).Draw(rt, "summary chars")),
		digest.WithCandidateLimit(rapid.IntRange(1, 20).Draw(rt, "candidate limit")),
		digest.WithDemoteAfter(rapid.IntRange(0, 6).Draw(rt, "demote after")),
	)
}

// TestDigestNeverExceedsItsBudget is the property the whole budget exists for: no
// draw of corpus, budget, quota or caps can produce a digest that costs more than
// it was allowed.
func TestDigestNeverExceedsItsBudget(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st, _ := propCorpus(rt)
		d, err := propBuilder(rt, st).Select(context.Background(), digest.Query(propScope))
		if err != nil {
			rt.Fatalf("select: %v", err)
		}
		var sum int
		for _, l := range d.Lines {
			sum += l.Tokens
		}
		if sum != d.Tokens {
			rt.Fatalf("Tokens = %d, lines cost %d", d.Tokens, sum)
		}
		if d.Tokens > d.Budget {
			rt.Fatalf("spent %d tokens against a budget of %d", d.Tokens, d.Budget)
		}
	})
}

// TestDigestNeverCarriesADeniedItem is the security property. Whatever the budget
// and quota do, a tainted or untrusted-origin item never reaches a line, and a
// line that needed a review has one.
func TestDigestNeverCarriesADeniedItem(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st, written := propCorpus(rt)
		d, err := propBuilder(rt, st).Select(context.Background(), digest.Query(propScope))
		if err != nil {
			rt.Fatalf("select: %v", err)
		}
		by := make(map[string]state.MemoryItem, len(written))
		for _, it := range written {
			by[it.ID] = it
		}
		promoted, err := st.Promotions(context.Background(), nil)
		if err != nil {
			rt.Fatalf("promotions: %v", err)
		}
		ok := state.PromotedSet(promoted)
		for _, l := range append(append([]digest.Line{}, d.Lines...), d.Dropped...) {
			it, found := by[l.MemoryID]
			if !found {
				rt.Fatalf("line %s names an item nobody wrote", l.MemoryID)
			}
			if !guard.PushEligible(it, ok[it.ID]) {
				rt.Fatalf("item %s reached the digest: tainted=%v sources=%v promoted=%v",
					it.ID, it.Tainted, it.Sources, ok[it.ID])
			}
		}
	})
}

// TestDigestAccountsForEveryCandidate: a candidate is either selected or reported
// as dropped, never silently gone, and no id appears twice.
func TestDigestAccountsForEveryCandidate(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st, _ := propCorpus(rt)
		d, err := propBuilder(rt, st).Select(context.Background(), digest.Query(propScope))
		if err != nil {
			rt.Fatalf("select: %v", err)
		}
		if got := len(d.Lines) + len(d.Dropped); got != d.Considered {
			rt.Fatalf("lines + dropped = %d, Considered = %d", got, d.Considered)
		}
		seen := map[string]bool{}
		for _, l := range append(append([]digest.Line{}, d.Lines...), d.Dropped...) {
			if seen[l.MemoryID] {
				rt.Fatalf("item %s appears twice", l.MemoryID)
			}
			seen[l.MemoryID] = true
		}
	})
}

// TestDigestKeepsScopeOrder: the lines are ordered most-specific scope first,
// whichever pass chose each of them, with the demoted ones after all of the rest.
// Recency within a level is the store's (state.SortRecall) and is not restated here.
func TestDigestKeepsScopeOrder(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st, _ := propCorpus(rt)
		d, err := propBuilder(rt, st).Select(context.Background(), digest.Query(propScope))
		if err != nil {
			rt.Fatalf("select: %v", err)
		}
		for i := 1; i < len(d.Lines); i++ {
			prev, cur := d.Lines[i-1], d.Lines[i]
			if prev.Demoted && !cur.Demoted {
				rt.Fatalf("line %d is demoted and line %d, which follows it, is not", i-1, i)
			}
			if prev.Demoted != cur.Demoted {
				continue
			}
			if prev.Scope.Depth() < cur.Scope.Depth() {
				rt.Fatalf("line %d (%v) outranks line %d (%v)",
					i, cur.Scope, i-1, prev.Scope)
			}
		}
	})
}

// TestDemotionOnlyRanks is the guarantee that makes a demotion recoverable: under
// any draw, demotion changes which items the budget reaches and never which items
// the selection was willing to reach. An item a policy stops offering is an item
// nobody can use, and its own demotion would then be the reason it never earns its
// way back.
func TestDemotionOnlyRanks(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st, _ := propCorpus(rt)
		budget := rapid.IntRange(1, 400).Draw(rt, "budget")
		q := digest.Query(propScope)

		on, err := digest.New(st, digest.WithBudget(budget)).Select(context.Background(), q)
		if err != nil {
			rt.Fatalf("select with demotion: %v", err)
		}
		off, err := digest.New(st, digest.WithBudget(budget), digest.WithDemoteAfter(0)).Select(context.Background(), q)
		if err != nil {
			rt.Fatalf("select without demotion: %v", err)
		}
		if a, b := candidateSet(on), candidateSet(off); !maps.Equal(a, b) {
			rt.Fatalf("demotion changed the candidate set: %v vs %v", a, b)
		}
		if on.Considered != off.Considered {
			rt.Fatalf("Considered = %d with demotion, %d without", on.Considered, off.Considered)
		}
	})
}

// candidateSet is every item the selection was willing to spend budget on: what it
// took and what it only ran out of room for.
func candidateSet(d digest.Digest) map[string]bool {
	out := make(map[string]bool, d.Considered)
	for _, l := range append(append([]digest.Line{}, d.Lines...), d.Dropped...) {
		out[l.MemoryID] = true
	}
	return out
}

// TestDigestStaysInTheQueriedScopeChain: nothing from a sibling scope is ever
// pushed, however the passes fill the budget.
func TestDigestStaysInTheQueriedScopeChain(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st, _ := propCorpus(rt)
		d, err := propBuilder(rt, st).Select(context.Background(), digest.Query(propScope))
		if err != nil {
			rt.Fatalf("select: %v", err)
		}
		chain := map[state.Scope]bool{}
		for _, s := range propScope.Ancestors() {
			chain[s] = true
		}
		for _, l := range d.Lines {
			if !chain[l.Scope] {
				rt.Fatalf("line %s is at %v, outside the queried chain", l.MemoryID, l.Scope)
			}
		}
	})
}

// TestBuildPushesExactlyWhatItSelected: the push is recorded for every line and
// for nothing else.
func TestBuildPushesExactlyWhatItSelected(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st, _ := propCorpus(rt)
		pusher := &countingPusher{}
		d, err := digest.New(st,
			digest.WithBudget(rapid.IntRange(1, 400).Draw(rt, "budget")),
			digest.WithExplorationQuota(rapid.Float64Range(0, 1).Draw(rt, "quota")),
			digest.WithPusher(pusher),
		).Build(context.Background(), digest.Query(propScope))
		if err != nil {
			rt.Fatalf("build: %v", err)
		}
		want := d.IDs()
		if len(want) == 0 {
			if pusher.calls != 0 {
				rt.Fatalf("pusher called %d times for an empty digest", pusher.calls)
			}
			return
		}
		if pusher.calls != 1 {
			rt.Fatalf("pusher called %d times, want once", pusher.calls)
		}
		if len(pusher.ids) != len(want) {
			rt.Fatalf("pushed %v, selected %v", pusher.ids, want)
		}
		for i := range want {
			if pusher.ids[i] != want[i] {
				rt.Fatalf("pushed %v, selected %v", pusher.ids, want)
			}
		}
	})
}

// TestSummarizeIsAlwaysOneCappedLine: whatever the content, a summary is a single
// line no longer than the cap.
func TestSummarizeIsAlwaysOneCappedLine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		content := rapid.String().Draw(rt, "content")
		limit := rapid.IntRange(1, 120).Draw(rt, "max")
		got := digest.Summarize(content, limit)
		if len(got) > limit {
			rt.Fatalf("Summarize(%q, %d) = %q, over the cap", content, limit, got)
		}
		for _, r := range got {
			if r == '\n' || r == '\r' || r == '\t' {
				rt.Fatalf("Summarize(%q, %d) = %q, not one line", content, limit, got)
			}
		}
	})
}
