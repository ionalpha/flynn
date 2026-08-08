package hybrid_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/memory/hybrid"
	"github.com/ionalpha/flynn/state"
)

// epoch starts every test's manual clock, so CreatedAt is what the test advanced
// it to and never the wall clock.
var epoch = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// corpus is the fixture memory these tests recall against. Each item is written
// in the wording of the day it was learned, and the queries below are worded the
// way somebody would ask months later.
var corpus = []state.MemoryItem{
	{Kind: "decision", Subject: "db-choice", Content: "we settled on Postgres for the primary datastore"},
	{Kind: "decision", Subject: "queue-choice", Content: "we chose RabbitMQ after the spike"},
	{Kind: "decision", Subject: "release-window", Content: "the release goes out Thursday afternoon"},
	{Kind: "preference", Subject: "editor-style", Content: "the user prefers tabs over spaces"},
	{Kind: "observation", Subject: "suite-health", Content: "the suite retries on timeout and hides failures"},
	{Kind: "note", Subject: "", Content: "the incident ticket is OPS-4821"},
}

type fixture struct {
	t     *testing.T
	store *hybrid.Store
	inner state.MemoryStore
	emb   *fakeEmbedder
	ids   map[string]string // subject or content prefix -> stored id
}

func newFixture(t *testing.T, opts ...hybrid.Option) *fixture {
	t.Helper()
	clk := clock.NewManual(epoch)
	p := state.NewMemory(state.WithClock(clk))
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Fatalf("close provider: %v", err)
		}
	})
	f := &fixture{t: t, inner: p.Memory(), emb: &fakeEmbedder{}, ids: map[string]string{}}
	f.store = hybrid.Wrap(f.inner, append([]hybrid.Option{hybrid.WithEmbedder(f.emb)}, opts...)...)
	for _, it := range corpus {
		out, err := f.inner.Write(context.Background(), it)
		if err != nil {
			t.Fatalf("write %q: %v", it.Content, err)
		}
		f.ids[key(it)] = out.ID
		clk.Advance(time.Minute)
	}
	return f
}

// key names a fixture item by its subject, or by its content when it has none.
func key(it state.MemoryItem) string {
	if it.Subject != "" {
		return it.Subject
	}
	return it.Content
}

func (f *fixture) recall(q state.RecallQuery) []state.MemoryItem {
	f.t.Helper()
	items, err := f.store.Recall(context.Background(), q)
	if err != nil {
		f.t.Fatalf("recall %q: %v", q.Query, err)
	}
	return items
}

// keys renders a result set as fixture keys, in order, for readable assertions.
func (f *fixture) keys(items []state.MemoryItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, key(it))
	}
	return out
}

func TestRecallFindsRewordedFact(t *testing.T) {
	f := newFixture(t)
	q := state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance}

	// The lexical measure on its own has nothing: the fact says "datastore" and
	// "settled", the question says "storage engine" and "choose", and they share no
	// word. This is the case the package exists for, so assert the baseline rather
	// than assuming it.
	lexical, err := f.inner.Recall(context.Background(), q)
	if err != nil {
		t.Fatalf("inner recall: %v", err)
	}
	if len(lexical) != 0 {
		t.Fatalf("lexical-only recall = %v, want no results", f.keys(lexical))
	}

	got := f.recall(q)
	if len(got) == 0 || key(got[0]) != "db-choice" {
		t.Fatalf("hybrid recall = %v, want db-choice first", f.keys(got))
	}
	if got[0].Score <= 0 || got[0].Score > 1 {
		t.Fatalf("score = %v, want it in (0,1]", got[0].Score)
	}
}

func TestRecallKeepsExactMatchOnTop(t *testing.T) {
	// An identifier is the case the embedding half is blind to: it is not a word,
	// so it points nowhere in the concept space, and only the lexical measure can
	// find it. Fusion must not let the items that are merely near the query in
	// meaning outrank the one that actually contains what was asked for.
	f := newFixture(t)
	got := f.recall(state.RecallQuery{Query: "OPS-4821", Order: state.OrderRelevance})
	if len(got) == 0 || key(got[0]) != "the incident ticket is OPS-4821" {
		t.Fatalf("recall = %v, want the ticket first", f.keys(got))
	}
}

func TestRecallCarriesSelectors(t *testing.T) {
	// Both inner reads keep every selector but the query text, so a kind filter
	// still holds over the half of the ranking the caller's words never reached.
	f := newFixture(t)
	got := f.recall(state.RecallQuery{
		Query: "which storage engine did we choose",
		Kinds: []string{"preference"},
		Order: state.OrderRelevance,
	})
	for _, it := range got {
		if it.Kind != "preference" {
			t.Fatalf("recall returned kind %q, want preference only: %v", it.Kind, f.keys(got))
		}
	}
}

func TestRecallRankAndFloorAndLimit(t *testing.T) {
	f := newFixture(t)
	q := state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance}
	full := f.recall(q)
	if len(full) < 2 {
		t.Fatalf("recall = %v, want at least two results to rank", f.keys(full))
	}
	for i, it := range full {
		if it.Score <= 0 || it.Score > 1 {
			t.Fatalf("score[%d] = %v, want it in (0,1]", i, it.Score)
		}
		if i > 0 && full[i-1].Score < it.Score {
			t.Fatalf("scores %v out of order at %d", full, i)
		}
	}

	// The floor is a floor on the fused score, so it cuts the tail of that ranking
	// and not of either measure's own.
	floor := full[len(full)-1].Score + 1e-9
	q.MinScore = floor
	floored := f.recall(q)
	if len(floored) >= len(full) {
		t.Fatalf("floored recall = %v, want fewer than %v", f.keys(floored), f.keys(full))
	}
	for _, it := range floored {
		if it.Score < floor {
			t.Fatalf("floored recall kept score %v, below %v", it.Score, floor)
		}
	}

	// The limit applies to the fused ranking, so it keeps the best item rather than
	// the best the lexical half found.
	q.MinScore = 0
	q.Limit = 1
	limited := f.recall(q)
	if len(limited) != 1 || key(limited[0]) != key(full[0]) {
		t.Fatalf("limited recall = %v, want just %q", f.keys(limited), key(full[0]))
	}
}

func TestRecallDelegatesWithoutQueryOrEmbedder(t *testing.T) {
	f := newFixture(t)
	// No query text is nothing to fuse: the anchored and structural reads are the
	// inner store's answer unchanged.
	empty := state.RecallQuery{Kinds: []string{"decision"}}
	got, want := f.recall(empty), mustRecall(t, f.inner, empty)
	if len(got) != len(want) {
		t.Fatalf("empty-query recall = %v, want the inner answer %v", f.keys(got), f.keys(want))
	}

	plain := hybrid.Wrap(f.inner)
	q := state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance}
	items, err := plain.Recall(context.Background(), q)
	if err != nil {
		t.Fatalf("recall without embedder: %v", err)
	}
	if len(items) != len(mustRecall(t, f.inner, q)) {
		t.Fatalf("recall without embedder = %v, want the inner answer", f.keys(items))
	}
	if calls, _ := f.emb.counts(); calls != 0 {
		t.Fatalf("embedder calls = %d, want none: neither read had anything to fuse", calls)
	}
}

func TestRecallDegradesWhenEmbedderFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeEmbedder)
	}{
		{"error", func(e *fakeEmbedder) { e.err = errEmbedder }},
		{"short list", func(e *fakeEmbedder) { e.short = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f.emb)
			// A term the lexical measure can match, so there is a usable answer to
			// return alongside the news that it was not fused.
			items, err := f.store.Recall(context.Background(), state.RecallQuery{Query: "Postgres", Order: state.OrderRelevance})
			if !errors.Is(err, hybrid.ErrNotFused) {
				t.Fatalf("err = %v, want ErrNotFused", err)
			}
			if len(items) != 1 || key(items[0]) != "db-choice" {
				t.Fatalf("degraded recall = %v, want the lexical hit", f.keys(items))
			}
		})
	}
}

func TestRecallCachesVectors(t *testing.T) {
	f := newFixture(t)
	q := state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance}
	f.recall(q)
	calls, texts := f.emb.counts()
	if calls != 1 || texts != len(corpus)+1 {
		t.Fatalf("first recall embedded (%d calls, %d texts), want (1, %d)", calls, texts, len(corpus)+1)
	}
	f.recall(q)
	if calls, texts = f.emb.counts(); calls != 1 || texts != len(corpus)+1 {
		t.Fatalf("second recall embedded (%d calls, %d texts), want the cache to answer it", calls, texts)
	}
}

func TestSubjectSimilarity(t *testing.T) {
	f := newFixture(t)
	sim := f.store.SubjectSimilarity(context.Background())

	// The pair the lexical measure in memory/curate cannot connect: one topic, no
	// shared token.
	synonymous := sim("db-choice", "storage-engine-decision")
	unrelated := sim("db-choice", "release-window")
	if synonymous <= unrelated {
		t.Fatalf("similarity: synonymous %v, unrelated %v, want the synonymous pair higher", synonymous, unrelated)
	}
	if synonymous < 0 || synonymous > 1 {
		t.Fatalf("similarity = %v, want it in [0,1]", synonymous)
	}
	if back := sim("storage-engine-decision", "db-choice"); back != synonymous {
		t.Fatalf("similarity is not symmetric: %v then %v", synonymous, back)
	}
	if same := sim("db-choice", "db-choice"); same != 1 {
		t.Fatalf("similarity of a subject with itself = %v, want 1", same)
	}
	if empty := sim("db-choice", ""); empty != 0 {
		t.Fatalf("similarity against an empty subject = %v, want 0", empty)
	}

	// No evidence is not evidence of a fork. The failing store is a fresh one:
	// this one has the pair cached and would answer without the model.
	broken := newFixture(t)
	broken.emb.err = errEmbedder
	if failed := broken.store.SubjectSimilarity(context.Background())("db-choice", "storage-engine-decision"); failed != 0 {
		t.Fatalf("similarity with a failing embedder = %v, want 0", failed)
	}
	if none := hybrid.Wrap(f.inner).SubjectSimilarity(context.Background())("db-choice", "storage-engine"); none != 0 {
		t.Fatalf("similarity with no embedder = %v, want 0", none)
	}
}

func TestOptionsRejectUselessValues(t *testing.T) {
	// Every option ignores a value that would make the ranking meaningless, so a
	// misconfigured host gets the default rather than a silently broken store.
	f := newFixture(t,
		hybrid.WithEmbedder(nil),
		hybrid.WithRankConstant(0),
		hybrid.WithWeights(0, 0),
		hybrid.WithWeights(-1, 2),
		hybrid.WithCandidates(0),
		hybrid.WithDepth(0),
		hybrid.WithCacheSize(0),
	)
	got := f.recall(state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance})
	if len(got) == 0 || key(got[0]) != "db-choice" {
		t.Fatalf("recall = %v, want the defaults still ranking db-choice first", f.keys(got))
	}
}

func TestWeightsSwitchAMeasureOff(t *testing.T) {
	// Lexical only: the reworded question finds nothing, which is the behaviour
	// this package replaces and the A/B baseline for measuring it.
	f := newFixture(t, hybrid.WithWeights(1, 0))
	q := state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance}
	if got := f.recall(q); len(got) != 0 {
		t.Fatalf("lexical-only recall = %v, want no results", f.keys(got))
	}

	// Embedding only: the identifier query loses the item that contains it,
	// because nothing points at an identifier in a meaning space.
	g := newFixture(t, hybrid.WithWeights(0, 1))
	for _, it := range g.recall(state.RecallQuery{Query: "OPS-4821", Order: state.OrderRelevance}) {
		if key(it) == "the incident ticket is OPS-4821" {
			t.Fatal("embedding-only recall found the ticket, so the fixture no longer tests the split")
		}
	}
}

func TestDelegatesEverythingElse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	written, err := f.store.Write(ctx, state.MemoryItem{Kind: "fact", Content: "the deploy needs a manual approval"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.store.RecordPush(ctx, []string{written.ID}); err != nil {
		t.Fatalf("record push: %v", err)
	}
	if err := f.store.RecordUse(ctx, written.ID, state.UsagePrimed); err != nil {
		t.Fatalf("record use: %v", err)
	}
	usage, err := f.store.Usage(ctx, []string{written.ID})
	if err != nil || len(usage) != 1 {
		t.Fatalf("usage = %v, %v, want one row", usage, err)
	}
	if _, err := f.store.Promote(ctx, state.PromotionDecision{MemoryID: written.ID, Promoted: true, By: "reviewer"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	promos, err := f.store.Promotions(ctx, []string{written.ID})
	if err != nil || len(promos) != 1 {
		t.Fatalf("promotions = %v, %v, want one row", promos, err)
	}
	if err := f.store.Delete(ctx, written.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := f.store.Delete(ctx, "no-such-id"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("delete of an unknown id = %v, want ErrNotFound", err)
	}
}

func TestTunedOptionsHold(t *testing.T) {
	q := state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance}
	wide := newFixture(t, hybrid.WithRankConstant(1), hybrid.WithWeights(3, 1), hybrid.WithCacheSize(64))
	if got := wide.recall(q); len(got) < 2 {
		t.Fatalf("recall = %v, want both of the decisions the question is near", wide.keys(got))
	}

	// The reading depth is how many of the pool count as matched by meaning, so at
	// one, only the nearest item does.
	narrow := newFixture(t, hybrid.WithDepth(1))
	if got := narrow.recall(q); len(got) != 1 || key(got[0]) != "db-choice" {
		t.Fatalf("recall at depth 1 = %v, want just db-choice", narrow.keys(got))
	}

	// The candidate cap bounds the pool, and the pool comes back in recency order,
	// so a cap below the corpus size hides the oldest items from the meaning half.
	capped := newFixture(t, hybrid.WithCandidates(2))
	for _, it := range capped.recall(q) {
		if key(it) == "db-choice" {
			t.Fatalf("recall = %v, want the oldest item outside a 2-item pool", capped.keys(capped.recall(q)))
		}
	}
}

func TestEqualMatchesRankDeterministically(t *testing.T) {
	// Two items saying the same thing in the same words are equally near any
	// query, and the store may list them in either order. The ranking breaks the
	// tie on the item ID, so a recall repeated against an unchanged corpus returns
	// the same order every time.
	f := newFixture(t)
	twin := state.MemoryItem{Kind: "note", Content: "we settled on the datastore"}
	for range 2 {
		if _, err := f.inner.Write(context.Background(), twin); err != nil {
			t.Fatalf("write twin: %v", err)
		}
	}
	q := state.RecallQuery{Query: "which storage engine did we choose", Order: state.OrderRelevance}
	first, second := f.recall(q), f.recall(q)
	if len(first) < 2 {
		t.Fatalf("recall = %v, want the twins in it", f.keys(first))
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].Score != second[i].Score {
			t.Fatalf("recall is not deterministic at %d: %v then %v", i, first[i], second[i])
		}
	}
}

func TestRecallSurfacesInnerErrors(t *testing.T) {
	// A store that cannot answer is a failed read, not a degraded ranking: there is
	// no result to hand back and pretending otherwise would report an empty memory.
	for _, failOn := range []int{1, 2} {
		f := newFixture(t)
		store := hybrid.Wrap(&failingStore{MemoryStore: f.inner, failOn: failOn}, hybrid.WithEmbedder(f.emb))
		_, err := store.Recall(context.Background(), state.RecallQuery{Query: "which storage engine did we choose"})
		if !errors.Is(err, errRecall) {
			t.Fatalf("recall with read %d failing = %v, want the store's error", failOn, err)
		}
	}
}

var errRecall = errors.New("store unavailable")

// failingStore fails the failOn'th recall it is asked for and delegates the rest,
// so a test can break either of the two reads a fused recall makes.
type failingStore struct {
	state.MemoryStore
	failOn int
	n      int
}

func (f *failingStore) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	f.n++
	if f.n == f.failOn {
		return nil, errRecall
	}
	return f.MemoryStore.Recall(ctx, q)
}

func mustRecall(t *testing.T, s state.MemoryStore, q state.RecallQuery) []state.MemoryItem {
	t.Helper()
	items, err := s.Recall(context.Background(), q)
	if err != nil {
		t.Fatalf("inner recall: %v", err)
	}
	return items
}
