package digest

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/memory/ridealong"
	"github.com/ionalpha/flynn/state"
)

// Defaults for a builder that is handed no options. They are working guesses,
// stated here so a host tuning one can see what it is moving away from.
const (
	// defaultBudget is the token budget one digest may spend. A few hundred tokens is
	// affordable on every wake of every session, which is the property that matters:
	// a digest expensive enough to think twice about is a digest that gets switched
	// off.
	defaultBudget = 512
	// defaultExplorationQuota is the share of the budget reserved for recent,
	// rarely-pushed items at the most specific scope. Without it the ordering is
	// stable, so the same items win every wake, get used because they are there, and
	// look like they earned it. A fifth is enough for a couple of lines out of a dozen.
	defaultExplorationQuota = 0.2
	// defaultSummaryChars caps one line's sentence. It is sized to read as one line in
	// a terminal, and the id behind it is how a reader gets the rest.
	defaultSummaryChars = 160
	// defaultCandidateLimit caps how many items one selection reads before choosing.
	// A digest of a dozen lines has no use for a thousand candidates, and an uncapped
	// read would make the cost of building it grow with the corpus forever.
	defaultCandidateLimit = 200
)

// ErrPushNotRecorded reports that a digest was built but the push could not be
// counted. It is returned alongside the digest, never instead of it: the lines are
// real and the session needs them, so failing the wake to protect the
// instrumentation would trade the product for the measurement. It mirrors
// ridealong.ErrUsageNotRecorded, which is the same call made from the pull side.
var ErrPushNotRecorded = errors.New("digest: push not recorded")

// Pusher records that a set of items reached a reader unasked. ridealong.Surfacer
// is one, and is the default: it counts the push in the store and marks the run's
// prime scope together, which is the pair that has to stay in step (see
// ridealong.Surfacer.Push).
type Pusher interface {
	Push(ctx context.Context, memoryIDs []string) error
}

// Builder selects and renders the wake digest. It holds no per-run state; what a
// run has already been shown lives on the context, in the prime scope the push
// marks.
type Builder struct {
	store      state.MemoryStore
	push       Pusher
	budget     int
	quota      float64
	summary    int
	candidates int
}

// Option configures a Builder.
type Option func(*Builder)

// WithBudget sets the token budget one digest may spend. A non-positive n restores
// the default. There is no unlimited setting: an unbudgeted digest is a context leak
// that grows with the corpus, and the caller that genuinely wants every eligible
// item is doing a recall.
func WithBudget(n int) Option {
	return func(b *Builder) {
		if n <= 0 {
			n = defaultBudget
		}
		b.budget = n
	}
}

// WithExplorationQuota sets the share of the budget reserved for recent,
// rarely-pushed items at the most specific scope, clamped to [0,1]. Zero turns
// exploration off and gives the whole budget to the standing order, which is what a
// host wants when it has its own diversity policy and not otherwise: see
// state.Monoculture for what a fleet without one converges on.
//
// The reserve is not a floor on the digest's variety, only on its budget. When
// there is nothing unexplored to spend it on it goes back to the standing order
// rather than shortening the digest.
func WithExplorationQuota(q float64) Option {
	return func(b *Builder) {
		switch {
		case q < 0:
			q = 0
		case q > 1:
			q = 1
		}
		b.quota = q
	}
}

// WithSummaryChars caps the sentence on one line. A non-positive n restores the
// default.
func WithSummaryChars(n int) Option {
	return func(b *Builder) {
		if n <= 0 {
			n = defaultSummaryChars
		}
		b.summary = n
	}
}

// WithCandidateLimit caps how many items one selection reads before choosing. A
// non-positive n restores the default. Raising it buys a deeper search of the
// corpus for a longer read on every wake; a selection that hit the cap says so
// (Digest.Capped) rather than reporting a full corpus it did not see.
func WithCandidateLimit(n int) Option {
	return func(b *Builder) {
		if n <= 0 {
			n = defaultCandidateLimit
		}
		b.candidates = n
	}
}

// WithPusher sets what records a delivered digest. The default counts the push in
// the store and marks the run's prime scope; a host that has already wired its own
// equivalent passes it here rather than getting both.
func WithPusher(p Pusher) Option {
	return func(b *Builder) {
		if p != nil {
			b.push = p
		}
	}
}

// New returns a Builder reading through store.
func New(store state.MemoryStore, opts ...Option) *Builder {
	b := &Builder{
		store:      store,
		push:       ridealong.New(store),
		budget:     defaultBudget,
		quota:      defaultExplorationQuota,
		summary:    defaultSummaryChars,
		candidates: defaultCandidateLimit,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Query is the canonical wake query for a scope: everything live at that scope and
// at the scopes enclosing it, ordered most-specific first. It exists so the widening
// is stated once here rather than re-derived, and correctly, by every host that
// builds a digest.
//
// A zero scope spans every scope, which is the right read for a single-scope install
// and the wrong one for a host that partitions its work; that host passes its scope.
func Query(scope state.Scope) state.RecallQuery {
	return state.RecallQuery{Scope: scope, IncludeAncestors: true}
}

// Build selects the digest for q and records the push of everything it selected, so
// a later use of a pushed item is attributed as primed rather than as the reader
// having found it unaided. It is the call a host makes at wake, inside a context
// carrying a prime scope (ridealong.NewPrimeScope).
//
// A digest that could not be counted still comes back, wrapped in
// ErrPushNotRecorded. An empty digest records nothing and cannot fail.
func (b *Builder) Build(ctx context.Context, q state.RecallQuery) (Digest, error) {
	d, err := b.Select(ctx, q)
	if err != nil {
		return Digest{}, err
	}
	ids := d.IDs()
	if len(ids) == 0 {
		return d, nil
	}
	if err := b.push.Push(ctx, ids); err != nil {
		return d, fmt.Errorf("%w: %w", ErrPushNotRecorded, err)
	}
	return d, nil
}

// Select chooses the digest for q without recording anything. It is the half a
// preview, a test or an operator's "what would we push" tool wants; a host handing
// the result to a session calls Build instead, or the numbers under-count from the
// first wake.
//
// Selection, in order:
//
//  1. Recall q, capped at the candidate limit, and drop everything
//     guard.Pushable does not admit. Nothing reaches a line without passing that gate.
//  2. Fill the budget less the exploration reserve, in recall order: most-specific
//     scope first, most recent within a level (state.SortRecall).
//  3. Fill the reserve from the items at the most specific scope that step 2 did
//     not take, rarely-pushed first and most recent within that.
//  4. Give whatever either pass left over back to the recall order, so an
//     under-spent reserve shortens nobody's digest.
//
// A line that does not fit is skipped and the pass continues, rather than ending the
// digest there. It is backfill, not displacement: an item is only passed over after
// it has been offered the budget and refused it, so what follows can only be smaller.
// Everything still unselected at the end is reported in Digest.Dropped.
//
// The returned lines are in recall order whichever pass chose them, so the digest
// reads as one ranked list and not as two concatenated ones.
func (b *Builder) Select(ctx context.Context, q state.RecallQuery) (Digest, error) {
	if q.Limit <= 0 || q.Limit > b.candidates {
		q.Limit = b.candidates
	}
	items, err := b.store.Recall(ctx, q)
	if err != nil {
		return Digest{}, err
	}
	capped := len(items) == q.Limit
	items, err = guard.Pushable(ctx, b.store, items)
	if err != nil {
		return Digest{}, err
	}
	lines := b.render(items)

	d := Digest{Budget: b.budget, Considered: len(lines), Capped: capped}
	reserve := int(float64(b.budget) * b.quota)
	taken := make([]bool, len(lines))
	left := b.budget - reserve

	left = fill(lines, taken, left, order(lines))
	explore, err := b.exploration(ctx, items, lines, taken)
	if err != nil {
		return Digest{}, err
	}
	spare := fill(lines, taken, reserve, explore)
	fill(lines, taken, left+spare, order(lines))

	for i, l := range lines {
		if taken[i] {
			d.Lines = append(d.Lines, l)
			d.Tokens += l.Tokens
			continue
		}
		d.Dropped = append(d.Dropped, l)
	}
	return d, nil
}

// render turns admitted items into candidate lines, in the order the recall
// returned them, dropping any whose content has nothing readable in it.
func (b *Builder) render(items []state.MemoryItem) []Line {
	out := make([]Line, 0, len(items))
	for _, it := range items {
		summary := Summarize(it.Content, b.summary)
		if !hasContent(summary) {
			continue
		}
		l := Line{MemoryID: it.ID, Kind: it.Kind, Scope: it.Scope, Summary: summary}
		l.Tokens = estimateTokens(l.Text())
		out = append(out, l)
	}
	return out
}

// order is the indices of lines in recall order, which is the order they are
// already in.
func order(lines []Line) []int {
	out := make([]int, len(lines))
	for i := range lines {
		out[i] = i
	}
	return out
}

// fill takes lines in the given order until the budget will not stretch to the next
// one, marking what it took, and returns what is left. A line already taken is
// skipped, so the same order can be replayed against a second budget.
func fill(lines []Line, taken []bool, budget int, idx []int) int {
	for _, i := range idx {
		if taken[i] || lines[i].Tokens > budget {
			continue
		}
		taken[i] = true
		budget -= lines[i].Tokens
	}
	return budget
}

// exploration returns the indices the exploration reserve may spend on: the
// still-unselected lines at the most specific scope present, rarely-pushed first,
// then most recent, then by id so the order is total.
//
// It is confined to the most specific scope because that is where new memory lands
// and where the standing order has the least to say: an inherited fact from a wider
// scope that keeps winning is usually winning on merit, while a fresh workspace note
// has no history at all and would never place against one. Ranking on push count
// rather than on use is the whole point - an item nobody has put in front of anybody
// has not failed, it has not been tried.
func (b *Builder) exploration(ctx context.Context, items []state.MemoryItem, lines []Line, taken []bool) ([]int, error) {
	if b.quota == 0 {
		return nil, nil
	}
	deepest := -1
	for i := range lines {
		if d := lines[i].Scope.Depth(); d > deepest {
			deepest = d
		}
	}
	var cand []int
	for i := range lines {
		if !taken[i] && lines[i].Scope.Depth() == deepest {
			cand = append(cand, i)
		}
	}
	if len(cand) == 0 {
		return nil, nil
	}
	pushes, err := b.pushCounts(ctx, lines, cand)
	if err != nil {
		return nil, err
	}
	// created is read off the recalled items rather than the lines, which carry no
	// timestamp: a line is what a reader sees, and a rendering does not need one.
	created := make(map[string]state.MemoryItem, len(items))
	for _, it := range items {
		created[it.ID] = it
	}
	sort.SliceStable(cand, func(x, y int) bool {
		a, c := lines[cand[x]], lines[cand[y]]
		if pushes[a.MemoryID] != pushes[c.MemoryID] {
			return pushes[a.MemoryID] < pushes[c.MemoryID]
		}
		ta, tc := created[a.MemoryID].CreatedAt, created[c.MemoryID].CreatedAt
		if !ta.Equal(tc) {
			return ta.After(tc)
		}
		return a.MemoryID < c.MemoryID
	})
	return cand, nil
}

// pushCounts reads the fleet-wide push count for the candidate lines. The count is
// summed across instances (state.TotalUsage) rather than read per instance, because
// an item this instance has never pushed but the rest of the fleet pushes constantly
// is not unexplored, and treating it as such would hand the exploration reserve to
// whatever is already everywhere.
func (b *Builder) pushCounts(ctx context.Context, lines []Line, cand []int) (map[string]int64, error) {
	ids := make([]string, 0, len(cand))
	for _, i := range cand {
		ids = append(ids, lines[i].MemoryID)
	}
	rows, err := b.store.Usage(ctx, ids)
	if err != nil {
		return nil, err
	}
	// An item with no row has never been pushed or used, which is zero and is what a
	// missing key already reports.
	out := make(map[string]int64, len(ids))
	for _, r := range rows {
		out[r.MemoryID] += r.PushCount
	}
	return out, nil
}
