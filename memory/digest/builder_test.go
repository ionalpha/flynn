package digest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory/digest"
	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
)

// epoch is the start of every test's manual clock, so CreatedAt is whatever the
// test advanced it to and never the wall clock.
var epoch = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// fixture is a memory store on a manual clock, with the helpers a selection test
// needs: write at a scope with a provenance, promote, and push.
type fixture struct {
	t     *testing.T
	store state.MemoryStore
	clk   *clock.Manual
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewManual(epoch)
	p := state.NewMemory(state.WithClock(clk))
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Fatalf("close provider: %v", err)
		}
	})
	return &fixture{t: t, store: p.Memory(), clk: clk}
}

// write persists an item and advances the clock, so every item in a fixture has a
// distinct CreatedAt and recency order is the write order reversed.
func (f *fixture) write(it state.MemoryItem) state.MemoryItem {
	f.t.Helper()
	out := f.writeAt(it)
	f.clk.Advance(time.Minute)
	return out
}

// writeAt persists an item without moving the clock, so a test can put two items
// at the same instant and see how a tie is broken.
func (f *fixture) writeAt(it state.MemoryItem) state.MemoryItem {
	f.t.Helper()
	if it.Kind == "" {
		it.Kind = "fact"
	}
	if len(it.Sources) == 0 {
		it.Sources = []string{"user:op"}
	}
	out, err := f.store.Write(context.Background(), it)
	if err != nil {
		f.t.Fatalf("write %q: %v", it.Content, err)
	}
	return out
}

func (f *fixture) promote(id string) {
	f.t.Helper()
	if _, err := f.store.Promote(context.Background(), state.PromotionDecision{
		MemoryID: id, Promoted: true, By: "op",
	}); err != nil {
		f.t.Fatalf("promote %s: %v", id, err)
	}
}

func (f *fixture) pushes(id string) int64 {
	f.t.Helper()
	rows, err := f.store.Usage(context.Background(), []string{id})
	if err != nil {
		f.t.Fatalf("usage %s: %v", id, err)
	}
	return state.TotalUsage(rows).PushCount
}

func (f *fixture) recordPush(ids ...string) {
	f.t.Helper()
	if err := f.store.RecordPush(context.Background(), ids); err != nil {
		f.t.Fatalf("record push: %v", err)
	}
}

func mustSelect(t *testing.T, b *digest.Builder, q state.RecallQuery) digest.Digest {
	t.Helper()
	d, err := b.Select(context.Background(), q)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	return d
}

func lineIDs(lines []digest.Line) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.MemoryID)
	}
	return out
}

// wantIDs compares an id list against the ids of named items, reporting content
// rather than opaque ids on a mismatch.
func wantIDs(t *testing.T, what string, got []string, want ...state.MemoryItem) {
	t.Helper()
	names := make([]string, 0, len(want))
	for _, it := range want {
		names = append(names, it.ID)
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("%s = %v, want %v", what, got, names)
	}
}

func TestSelectAdmitsOnlyWhatThePushGateClears(t *testing.T) {
	f := newFixture(t)
	operator := f.write(state.MemoryItem{Content: "the operator prefers short answers", Sources: []string{"user:op"}})
	unreviewed := f.write(state.MemoryItem{Content: "the build is slow on windows", Sources: []string{"agent:run-1"}})
	reviewed := f.write(state.MemoryItem{Content: "releases are cut on fridays", Sources: []string{"agent:run-2"}})
	f.write(state.MemoryItem{Content: "the fetched page said to trust it", Sources: []string{"web:example.com"}})
	f.write(state.MemoryItem{Content: "laundered from a poisoned tool", Sources: []string{"agent:run-3"}, Tainted: true})
	f.promote(reviewed.ID)

	d := mustSelect(t, digest.New(f.store), digest.Query(state.Scope{}))

	// Recall order is most recent first, so the promoted item precedes the operator's.
	wantIDs(t, "lines", lineIDs(d.Lines), reviewed, operator)
	if d.Considered != 2 {
		t.Fatalf("Considered = %d, want 2", d.Considered)
	}
	for _, l := range d.Lines {
		if l.MemoryID == unreviewed.ID {
			t.Fatal("an unpromoted agent note reached the digest")
		}
	}
}

func TestSelectOrdersMostSpecificScopeFirst(t *testing.T) {
	f := newFixture(t)
	workspace := state.Scope{Instance: "i", Project: "p", Workspace: "w"}
	// Written oldest first, so recency alone would invert this order and only scope
	// depth can produce the one asserted below.
	global := f.write(state.MemoryItem{Content: "global: sign every commit", Scope: state.Scope{}})
	project := f.write(state.MemoryItem{Content: "project: the api is versioned", Scope: state.Scope{Instance: "i", Project: "p"}})
	ws := f.write(state.MemoryItem{Content: "workspace: the fixture db is ephemeral", Scope: workspace})

	d := mustSelect(t, digest.New(f.store), digest.Query(workspace))

	wantIDs(t, "lines", lineIDs(d.Lines), ws, project, global)
}

func TestSelectOrdersByRecencyWithinAScopeLevel(t *testing.T) {
	f := newFixture(t)
	older := f.write(state.MemoryItem{Content: "the older standing fact about deploys"})
	newer := f.write(state.MemoryItem{Content: "the newer standing fact about deploys"})

	d := mustSelect(t, digest.New(f.store, digest.WithExplorationQuota(0)), digest.Query(state.Scope{}))

	wantIDs(t, "lines", lineIDs(d.Lines), newer, older)
}

func TestSelectStaysWithinTheBudgetAndReportsWhatItDropped(t *testing.T) {
	f := newFixture(t)
	for i := range 8 {
		f.write(state.MemoryItem{Content: fmt.Sprintf("standing fact number %d about how the deploy pipeline behaves", i)})
	}

	full := mustSelect(t, digest.New(f.store, digest.WithBudget(10_000)), digest.Query(state.Scope{}))
	if len(full.Lines) != 8 {
		t.Fatalf("unbudgeted digest has %d lines, want 8", len(full.Lines))
	}

	// Half the cost of the whole set, so roughly half of it has to be left out.
	budget := full.Tokens / 2
	d := mustSelect(t, digest.New(f.store, digest.WithBudget(budget), digest.WithExplorationQuota(0)), digest.Query(state.Scope{}))

	if d.Tokens > budget {
		t.Fatalf("spent %d tokens against a budget of %d", d.Tokens, budget)
	}
	if len(d.Lines) == 0 || len(d.Dropped) == 0 {
		t.Fatalf("lines = %d, dropped = %d, want both non-empty", len(d.Lines), len(d.Dropped))
	}
	if got := len(d.Lines) + len(d.Dropped); got != d.Considered {
		t.Fatalf("lines + dropped = %d, Considered = %d", got, d.Considered)
	}
	if d.Budget != budget {
		t.Fatalf("Budget = %d, want %d", d.Budget, budget)
	}
	// The dropped set is the tail of the ranking, not an arbitrary slice of it.
	wantIDs(t, "dropped", lineIDs(d.Dropped), byID(lineIDs(full.Lines)[len(d.Lines):])...)
}

// byID wraps bare ids as items so a want-list computed from a previous selection
// can be passed to wantIDs, which otherwise takes the items a test wrote.
func byID(ids []string) []state.MemoryItem {
	out := make([]state.MemoryItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, state.MemoryItem{ID: id})
	}
	return out
}

func TestSelectIsDeterministic(t *testing.T) {
	f := newFixture(t)
	for i := range 12 {
		f.write(state.MemoryItem{Content: fmt.Sprintf("fact %d about the release process and what it needs", i)})
	}
	b := digest.New(f.store, digest.WithBudget(120))

	first := mustSelect(t, b, digest.Query(state.Scope{}))
	for range 5 {
		next := mustSelect(t, b, digest.Query(state.Scope{}))
		if first.Text() != next.Text() {
			t.Fatalf("selection is not deterministic:\n%s\n---\n%s", first.Text(), next.Text())
		}
	}
}

func TestSelectBackfillsPastALineThatDoesNotFit(t *testing.T) {
	f := newFixture(t)
	// The newest item is far too long for the budget below; the two behind it fit.
	long := f.write(state.MemoryItem{Content: "short one about the api"})
	short := f.write(state.MemoryItem{Content: "short two about the api"})
	huge := f.write(state.MemoryItem{Content: strings.Repeat("a very long standing fact ", 40)})

	sized := mustSelect(t, digest.New(f.store, digest.WithBudget(10_000), digest.WithExplorationQuota(0)), digest.Query(state.Scope{}))
	var hugeTokens, shortTokens int
	for _, l := range sized.Lines {
		switch l.MemoryID {
		case huge.ID:
			hugeTokens = l.Tokens
		case short.ID:
			shortTokens = l.Tokens
		}
	}
	if hugeTokens <= shortTokens*2 {
		t.Fatalf("fixture is wrong: huge=%d short=%d", hugeTokens, shortTokens)
	}

	budget := hugeTokens - 1
	d := mustSelect(t, digest.New(f.store, digest.WithBudget(budget), digest.WithExplorationQuota(0)), digest.Query(state.Scope{}))

	wantIDs(t, "lines", lineIDs(d.Lines), short, long)
	wantIDs(t, "dropped", lineIDs(d.Dropped), huge)
	if d.Tokens > budget {
		t.Fatalf("spent %d tokens against a budget of %d", d.Tokens, budget)
	}
}

func TestExplorationQuotaReachesARarelyPushedItem(t *testing.T) {
	f := newFixture(t)
	workspace := state.Scope{Instance: "i", Project: "p", Workspace: "w"}
	// Four items at the workspace scope. The three newest have been pushed many
	// times; the oldest never has, so only the exploration reserve can reach it.
	fresh := f.write(state.MemoryItem{Content: "never pushed: the fixture db is ephemeral", Scope: workspace})
	var pushed []state.MemoryItem
	for i := range 3 {
		it := f.write(state.MemoryItem{Content: fmt.Sprintf("pushed often %d: the api is versioned here", i), Scope: workspace})
		pushed = append(pushed, it)
	}
	for range 5 {
		f.recordPush(pushed[0].ID, pushed[1].ID, pushed[2].ID)
	}

	full := mustSelect(t, digest.New(f.store, digest.WithBudget(10_000)), digest.Query(workspace))
	// A budget that fits three of the four lines, so the standing order alone would
	// take the three recent ones and never reach the oldest.
	budget := full.Tokens - full.Lines[len(full.Lines)-1].Tokens

	standing := mustSelect(t, digest.New(f.store, digest.WithBudget(budget), digest.WithExplorationQuota(0)), digest.Query(workspace))
	if contains(lineIDs(standing.Lines), fresh.ID) {
		t.Fatal("fixture is wrong: the standing order already reaches the unpushed item")
	}

	d := mustSelect(t, digest.New(f.store, digest.WithBudget(budget), digest.WithExplorationQuota(0.4)), digest.Query(workspace))

	if !contains(lineIDs(d.Lines), fresh.ID) {
		t.Fatalf("the exploration reserve did not reach the unpushed item: %v", lineIDs(d.Lines))
	}
	if d.Tokens > budget {
		t.Fatalf("spent %d tokens against a budget of %d", d.Tokens, budget)
	}
	// Whichever pass chose them, the lines read in recall order.
	wantRecallOrder(t, d, full)
}

func TestExplorationStaysAtTheMostSpecificScope(t *testing.T) {
	f := newFixture(t)
	workspace := state.Scope{Instance: "i", Project: "p", Workspace: "w"}
	// An old, never-pushed item at the wider scope is exactly what the reserve would
	// grab if it were not confined to the most specific one.
	outer := f.write(state.MemoryItem{Content: "project wide and never pushed once", Scope: state.Scope{Instance: "i", Project: "p"}})
	inner := f.write(state.MemoryItem{Content: "workspace local and pushed a lot", Scope: workspace})
	f.recordPush(inner.ID)

	budget := mustSelect(t, digest.New(f.store, digest.WithBudget(10_000)), digest.Query(workspace)).Lines[0].Tokens
	d := mustSelect(t, digest.New(f.store, digest.WithBudget(budget), digest.WithExplorationQuota(1)), digest.Query(workspace))

	wantIDs(t, "lines", lineIDs(d.Lines), inner)
	wantIDs(t, "dropped", lineIDs(d.Dropped), outer)
}

func TestUnspentReserveGoesBackToTheStandingOrder(t *testing.T) {
	f := newFixture(t)
	// Every item is at one scope and none has ever been pushed, so the reserve and
	// the standing order want the same rows and the reserve cannot be spent twice.
	for i := range 6 {
		f.write(state.MemoryItem{Content: fmt.Sprintf("fact %d about the release process and what it needs", i)})
	}
	q := digest.Query(state.Scope{})
	full := mustSelect(t, digest.New(f.store, digest.WithBudget(10_000)), q)
	budget := full.Tokens

	reserved := mustSelect(t, digest.New(f.store, digest.WithBudget(budget), digest.WithExplorationQuota(0.5)), q)
	if len(reserved.Lines) != len(full.Lines) {
		t.Fatalf("a half-reserved budget produced %d lines, want the full %d", len(reserved.Lines), len(full.Lines))
	}
	wantRecallOrder(t, reserved, full)
}

func TestSelectSkipsContentWithNothingReadable(t *testing.T) {
	f := newFixture(t)
	readable := f.write(state.MemoryItem{Content: "the operator prefers short answers"})
	f.write(state.MemoryItem{Content: "   "})
	f.write(state.MemoryItem{Content: "--- ... ---"})

	d := mustSelect(t, digest.New(f.store), digest.Query(state.Scope{}))

	wantIDs(t, "lines", lineIDs(d.Lines), readable)
	if d.Considered != 1 {
		t.Fatalf("Considered = %d, want 1: an unreadable item is not a candidate", d.Considered)
	}
}

func TestSelectReportsWhenTheCandidateReadWasCapped(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		f.write(state.MemoryItem{Content: fmt.Sprintf("fact %d about the deploy", i)})
	}
	q := digest.Query(state.Scope{})

	if d := mustSelect(t, digest.New(f.store, digest.WithCandidateLimit(10)), q); d.Capped {
		t.Fatal("Capped = true on a read that saw the whole corpus")
	}
	d := mustSelect(t, digest.New(f.store, digest.WithCandidateLimit(3)), q)
	if !d.Capped {
		t.Fatal("Capped = false on a read that hit its limit")
	}
	if d.Considered != 3 {
		t.Fatalf("Considered = %d, want the 3 the capped read saw", d.Considered)
	}
}

func TestSelectHonoursACallersTighterLimit(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		f.write(state.MemoryItem{Content: fmt.Sprintf("fact %d about the deploy", i)})
	}
	q := digest.Query(state.Scope{})
	q.Limit = 2

	d := mustSelect(t, digest.New(f.store), q)

	if d.Considered != 2 {
		t.Fatalf("Considered = %d, want the caller's limit of 2", d.Considered)
	}
}

func TestBuildRecordsThePushAndMarksThePrimeScope(t *testing.T) {
	f := newFixture(t)
	first := f.write(state.MemoryItem{Content: "the operator prefers short answers"})
	second := f.write(state.MemoryItem{Content: "releases are cut on fridays"})

	ctx := ridealong.NewPrimeScope(context.Background())
	d, err := digest.New(f.store).Build(ctx, digest.Query(state.Scope{}))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(d.Lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(d.Lines))
	}
	for _, it := range []state.MemoryItem{first, second} {
		if got := f.pushes(it.ID); got != 1 {
			t.Fatalf("push count for %s = %d, want 1", it.ID, got)
		}
		if !ridealong.Primed(ctx, it.ID) {
			t.Fatalf("%s was pushed but not marked on the prime scope", it.ID)
		}
	}
	rows, err := f.store.Usage(context.Background(), []string{first.ID})
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if state.TotalUsage(rows).LastPushedAt.IsZero() {
		t.Fatal("LastPushedAt is zero after a push")
	}
	if state.TotalUsage(rows).UseCount() != 0 {
		t.Fatal("a push counted as a use")
	}
}

func TestBuildDoesNotCountWhatTheBudgetDropped(t *testing.T) {
	f := newFixture(t)
	kept := f.write(state.MemoryItem{Content: "the operator prefers short answers here"})
	dropped := f.write(state.MemoryItem{Content: strings.Repeat("a very long standing fact ", 40)})

	full := mustSelect(t, digest.New(f.store, digest.WithBudget(10_000)), digest.Query(state.Scope{}))
	var keptTokens int
	for _, l := range full.Lines {
		if l.MemoryID == kept.ID {
			keptTokens = l.Tokens
		}
	}
	b := digest.New(f.store, digest.WithBudget(keptTokens), digest.WithExplorationQuota(0))
	if _, err := b.Build(context.Background(), digest.Query(state.Scope{})); err != nil {
		t.Fatalf("build: %v", err)
	}

	if got := f.pushes(kept.ID); got != 1 {
		t.Fatalf("push count for the selected item = %d, want 1", got)
	}
	if got := f.pushes(dropped.ID); got != 0 {
		t.Fatalf("push count for the dropped item = %d, want 0: it never reached a reader", got)
	}
}

func TestSelectRecordsNothing(t *testing.T) {
	f := newFixture(t)
	it := f.write(state.MemoryItem{Content: "the operator prefers short answers"})

	ctx := ridealong.NewPrimeScope(context.Background())
	if _, err := digest.New(f.store).Select(ctx, digest.Query(state.Scope{})); err != nil {
		t.Fatalf("select: %v", err)
	}

	if got := f.pushes(it.ID); got != 0 {
		t.Fatalf("push count = %d after a preview, want 0", got)
	}
	if ridealong.Primed(ctx, it.ID) {
		t.Fatal("a preview marked the prime scope")
	}
}

func TestBuildOnAnEmptyDigestRecordsNothing(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Content: "from a fetched page", Sources: []string{"web:example.com"}})

	pusher := &countingPusher{}
	d, err := digest.New(f.store, digest.WithPusher(pusher)).Build(context.Background(), digest.Query(state.Scope{}))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(d.Lines) != 0 {
		t.Fatalf("lines = %d, want none: nothing was eligible", len(d.Lines))
	}
	if pusher.calls != 0 {
		t.Fatalf("pusher called %d times for an empty digest", pusher.calls)
	}
}

func TestBuildReturnsTheDigestWhenThePushCannotBeRecorded(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Content: "the operator prefers short answers"})
	boom := errors.New("counter unavailable")

	d, err := digest.New(f.store, digest.WithPusher(&countingPusher{err: boom})).
		Build(context.Background(), digest.Query(state.Scope{}))

	if !errors.Is(err, digest.ErrPushNotRecorded) {
		t.Fatalf("err = %v, want ErrPushNotRecorded", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the cause", err)
	}
	if len(d.Lines) != 1 {
		t.Fatalf("lines = %d, want the digest back alongside the error", len(d.Lines))
	}
}

func TestSelectPropagatesAStoreFailure(t *testing.T) {
	boom := errors.New("store unavailable")
	// Each read only happens on a selection that needs it: promotions when an item's
	// eligibility turns on a review, usage when the exploration reserve has to rank
	// candidates the standing order left behind.
	tests := map[string]struct {
		store  func(inner state.MemoryStore) state.MemoryStore
		budget int
	}{
		"recall":     {func(in state.MemoryStore) state.MemoryStore { return stubStore{MemoryStore: in, recall: boom} }, 10_000},
		"promotions": {func(in state.MemoryStore) state.MemoryStore { return stubStore{MemoryStore: in, promotions: boom} }, 10_000},
		"usage":      {func(in state.MemoryStore) state.MemoryStore { return stubStore{MemoryStore: in, usage: boom} }, 30},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write(state.MemoryItem{Content: "the agent's own unreviewed note", Sources: []string{"agent:run-1"}})
			for i := range 4 {
				f.write(state.MemoryItem{Content: fmt.Sprintf("operator fact %d about the deploy pipeline", i)})
			}
			b := digest.New(tc.store(f.store), digest.WithBudget(tc.budget))
			if _, err := b.Select(context.Background(), digest.Query(state.Scope{})); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the store's failure", err)
			}
		})
	}
}

func TestBuildPropagatesASelectionFailure(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Content: "the operator prefers short answers"})
	boom := errors.New("store unavailable")
	pusher := &countingPusher{}

	st := stubStore{MemoryStore: f.store, recall: boom}
	d, err := digest.New(st, digest.WithPusher(pusher)).Build(context.Background(), digest.Query(state.Scope{}))

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if len(d.Lines) != 0 {
		t.Fatalf("lines = %d, want nothing back from a failed selection", len(d.Lines))
	}
	if pusher.calls != 0 {
		t.Fatalf("pusher called %d times after a failed selection", pusher.calls)
	}
}

func TestExplorationBreaksAnExactTieByID(t *testing.T) {
	f := newFixture(t)
	scope := state.Scope{Instance: "i"}
	// Two never-pushed items written at the same instant: the reserve can only order
	// them by id, and it has to do so the same way every time.
	a := f.writeAt(state.MemoryItem{Content: "tied one about the deploy pipeline", Scope: scope})
	b := f.writeAt(state.MemoryItem{Content: "tied two about the deploy pipeline", Scope: scope})
	if !a.CreatedAt.Equal(b.CreatedAt) {
		t.Fatalf("fixture is wrong: %v != %v", a.CreatedAt, b.CreatedAt)
	}
	first, second := a, b
	if b.ID < a.ID {
		first, second = b, a
	}

	// A reserve that fits exactly one line, and no budget for the standing order.
	budget := mustSelect(t, digest.New(f.store, digest.WithBudget(10_000)), digest.Query(scope)).Lines[0].Tokens
	d := mustSelect(t, digest.New(f.store, digest.WithBudget(budget), digest.WithExplorationQuota(1)), digest.Query(scope))

	wantIDs(t, "lines", lineIDs(d.Lines), first)
	wantIDs(t, "dropped", lineIDs(d.Dropped), second)
}

func TestOptionsFallBackToTheirDefaults(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.write(state.MemoryItem{Content: fmt.Sprintf("fact %d about the deploy pipeline", i)})
	}
	q := digest.Query(state.Scope{})

	base := mustSelect(t, digest.New(f.store), q)
	zeroed := mustSelect(t, digest.New(f.store,
		digest.WithBudget(0),
		digest.WithSummaryChars(-1),
		digest.WithCandidateLimit(0),
		digest.WithPusher(nil),
	), q)

	if base.Text() != zeroed.Text() {
		t.Fatalf("non-positive options changed the selection:\n%s\n---\n%s", base.Text(), zeroed.Text())
	}
	if zeroed.Budget != base.Budget {
		t.Fatalf("Budget = %d, want the default %d", zeroed.Budget, base.Budget)
	}
}

func TestExplorationQuotaIsClamped(t *testing.T) {
	f := newFixture(t)
	for i := range 4 {
		f.write(state.MemoryItem{Content: fmt.Sprintf("fact %d about the deploy pipeline", i)})
	}
	q := digest.Query(state.Scope{})

	if got, want := mustSelect(t, digest.New(f.store, digest.WithExplorationQuota(-1)), q).Text(),
		mustSelect(t, digest.New(f.store, digest.WithExplorationQuota(0)), q).Text(); got != want {
		t.Fatalf("a negative quota is not zero:\n%s\n---\n%s", got, want)
	}
	if got, want := mustSelect(t, digest.New(f.store, digest.WithExplorationQuota(2)), q).Text(),
		mustSelect(t, digest.New(f.store, digest.WithExplorationQuota(1)), q).Text(); got != want {
		t.Fatalf("a quota above one is not one:\n%s\n---\n%s", got, want)
	}
}

func TestSummaryCharsCapsTheLine(t *testing.T) {
	f := newFixture(t)
	f.write(state.MemoryItem{Content: "the operator wants every release announced in the channel before it ships"})

	d := mustSelect(t, digest.New(f.store, digest.WithSummaryChars(20)), digest.Query(state.Scope{}))

	if len(d.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(d.Lines))
	}
	if got := len(d.Lines[0].Summary); got > 20 {
		t.Fatalf("summary is %d chars, want at most 20: %q", got, d.Lines[0].Summary)
	}
}

func TestLinesCarryTheItemsKindAndScope(t *testing.T) {
	f := newFixture(t)
	scope := state.Scope{Instance: "i", Project: "p"}
	f.write(state.MemoryItem{Kind: "preference", Content: "short answers", Scope: scope})

	d := mustSelect(t, digest.New(f.store), digest.Query(scope))

	if len(d.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(d.Lines))
	}
	if got := d.Lines[0]; got.Kind != "preference" || got.Scope != scope {
		t.Fatalf("line = %+v, want kind=preference scope=%+v", got, scope)
	}
	if d.Lines[0].Tokens <= 0 {
		t.Fatalf("line costs %d tokens, want a positive estimate", d.Lines[0].Tokens)
	}
}

// wantRecallOrder asserts that d's lines appear in the same relative order as the
// unbudgeted selection full, whichever pass chose each of them.
func wantRecallOrder(t *testing.T, d, full digest.Digest) {
	t.Helper()
	rank := make(map[string]int, len(full.Lines))
	for i, l := range full.Lines {
		rank[l.MemoryID] = i
	}
	prev := -1
	for _, l := range d.Lines {
		r, ok := rank[l.MemoryID]
		if !ok {
			t.Fatalf("line %s is not in the unbudgeted selection", l.MemoryID)
		}
		if r <= prev {
			t.Fatalf("lines are out of recall order: %v", lineIDs(d.Lines))
		}
		prev = r
	}
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// countingPusher stands in for the default pusher so a test can see how often a
// push was attempted, and inject a failure.
type countingPusher struct {
	calls int
	ids   []string
	err   error
}

func (p *countingPusher) Push(_ context.Context, memoryIDs []string) error {
	p.calls++
	p.ids = append(p.ids, memoryIDs...)
	return p.err
}

// stubStore overrides one method of a real store so a failure can be injected
// without reimplementing the whole interface.
type stubStore struct {
	state.MemoryStore
	recall     error
	promotions error
	usage      error
}

func (s stubStore) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	if s.recall != nil {
		return nil, s.recall
	}
	return s.MemoryStore.Recall(ctx, q)
}

func (s stubStore) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	if s.promotions != nil {
		return nil, s.promotions
	}
	return s.MemoryStore.Promotions(ctx, memoryIDs)
}

func (s stubStore) Usage(ctx context.Context, memoryIDs []string) ([]state.MemoryUsage, error) {
	if s.usage != nil {
		return nil, s.usage
	}
	return s.MemoryStore.Usage(ctx, memoryIDs)
}
