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
	// defaultDemoteAfter is how many pushes an item may go unused for before it loses
	// the standing order. It is a small number because the evidence is one-sided: a
	// session that did not use a line it was shown may simply not have needed it that
	// wake, but five wakes in front of five readers with nothing acted on is the item
	// saying what it is. Below about three the threshold reads noise as a verdict.
	defaultDemoteAfter = 5
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
	demote     int
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

// WithDemoteAfter sets how many pushes an item may go unused for before it is
// demoted: ranked behind everything still earning its place, and passed over by the
// exploration reserve. A non-positive n turns demotion off, which leaves push_count
// measured and unread and the standing order deciding alone.
//
// Demotion is a ranking, never an exclusion. A demoted item still takes whatever
// budget the standing order leaves, so it keeps reaching readers and can still be
// used, and one use ends the demotion (state.MemoryUsage.Ignored). Dropping it from
// the digest instead would be one-way: an item nobody is shown is an item nobody can
// use, so its own demotion would be the reason it never earns its way back. The same
// reasoning is why nothing here deletes: what to do about an item the fleet has
// ignored for months is the curator's call, and this is a selection policy.
func WithDemoteAfter(n int) Option {
	return func(b *Builder) {
		if n < 0 {
			n = 0
		}
		b.demote = n
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
		demote:     defaultDemoteAfter,
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
//  2. Rank into the standing order: recall order (most-specific scope first, most
//     recent within a level, state.SortRecall), with everything demoted moved behind
//     everything else (WithDemoteAfter).
//  3. Fill the budget less the exploration reserve, in the standing order.
//  4. Fill the reserve from the undemoted items at the most specific scope that step
//     3 did not take, rarely-pushed first and most recent within that.
//  5. Give whatever either pass left over back to the standing order, so an
//     under-spent reserve shortens nobody's digest.
//
// A line that does not fit is skipped and the pass continues, rather than ending the
// digest there. It is backfill, not displacement: an item is only passed over after
// it has been offered the budget and refused it, so what follows can only be smaller.
// Everything still unselected at the end is reported in Digest.Dropped.
//
// The returned lines are in the standing order whichever pass chose them, so the
// digest reads as one ranked list and not as two concatenated ones, and the order a
// reader sees is the order the selection actually ranked on.
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
	usage, err := b.usage(ctx, lines)
	if err != nil {
		return Digest{}, err
	}
	b.markDemoted(lines, usage)
	ranked := standing(lines)

	d := Digest{Budget: b.budget, Considered: len(lines), Capped: capped}
	reserve := int(float64(b.budget) * b.quota)
	taken := make([]bool, len(lines))
	left := b.budget - reserve

	left = fill(lines, taken, left, ranked)
	spare := fill(lines, taken, reserve, b.exploration(items, lines, taken, usage))
	fill(lines, taken, left+spare, ranked)

	for _, i := range ranked {
		if taken[i] {
			d.Lines = append(d.Lines, lines[i])
			d.Tokens += lines[i].Tokens
			continue
		}
		d.Dropped = append(d.Dropped, lines[i])
	}
	return d, nil
}

// usage reads the fleet-wide usage total for every candidate line, in one call, for
// both policies that ask about it: demotion and the exploration reserve. The rows
// are summed across instances (state.TotalUsage) rather than read per instance,
// because an item this instance has never pushed but the rest of the fleet pushes
// constantly is not unexplored, and an item this instance has never seen used may
// have been used everywhere else.
//
// A builder with both policies off reads nothing. That keeps the wake off a store
// call whose answer it would discard, and off failing a digest on a question the
// selection is not asking.
func (b *Builder) usage(ctx context.Context, lines []Line) (map[string]state.MemoryUsage, error) {
	if len(lines) == 0 || (b.quota == 0 && b.demote == 0) {
		return nil, nil
	}
	ids := make([]string, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.MemoryID)
	}
	rows, err := b.store.Usage(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string][]state.MemoryUsage, len(ids))
	for _, r := range rows {
		byID[r.MemoryID] = append(byID[r.MemoryID], r)
	}
	// An item with no row has never been pushed or used, which is the zero total a
	// missing key already reports.
	out := make(map[string]state.MemoryUsage, len(byID))
	for id, rs := range byID {
		out[id] = state.TotalUsage(rs)
	}
	return out, nil
}

// markDemoted flags the lines pushed at least the threshold and never used, of
// either origin. The test is state.MemoryUsage.Ignored plus the threshold: one use,
// even a primed one, clears the flag, because a primed use still says a reader read
// the line and did something with it. What a primed use cannot do is earn an item a
// place it did not already hold, and it does not: this only ever lifts a demotion,
// never awards a promotion.
func (b *Builder) markDemoted(lines []Line, usage map[string]state.MemoryUsage) {
	if b.demote == 0 {
		return
	}
	for i := range lines {
		u := usage[lines[i].MemoryID]
		lines[i].Demoted = u.Ignored() && u.PushCount >= int64(b.demote)
	}
}

// standing is the indices of lines in the standing order: recall order, with the
// demoted lines after all of it and in recall order among themselves. A demotion
// costs an item every place it held, and no more than that.
func standing(lines []Line) []int {
	out := make([]int, 0, len(lines))
	for i := range lines {
		if !lines[i].Demoted {
			out = append(out, i)
		}
	}
	for i := range lines {
		if lines[i].Demoted {
			out = append(out, i)
		}
	}
	return out
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
// still-unselected, undemoted lines at the most specific scope present, rarely-pushed
// first, then most recent, then by id so the order is total.
//
// It is confined to the most specific scope because that is where new memory lands
// and where the standing order has the least to say: an inherited fact from a wider
// scope that keeps winning is usually winning on merit, while a fresh workspace note
// has no history at all and would never place against one. Ranking on push count
// rather than on use is the whole point - an item nobody has put in front of anybody
// has not failed, it has not been tried.
//
// A demoted item has been tried, repeatedly, which is why the reserve skips it. It is
// still offered the leftover budget in the standing order afterwards, so the reserve
// declining to spend on it costs it nothing it had not already lost.
func (b *Builder) exploration(items []state.MemoryItem, lines []Line, taken []bool, usage map[string]state.MemoryUsage) []int {
	if b.quota == 0 {
		return nil
	}
	deepest := -1
	for i := range lines {
		if d := lines[i].Scope.Depth(); d > deepest {
			deepest = d
		}
	}
	var cand []int
	for i := range lines {
		if !taken[i] && !lines[i].Demoted && lines[i].Scope.Depth() == deepest {
			cand = append(cand, i)
		}
	}
	if len(cand) == 0 {
		return nil
	}
	// created is read off the recalled items rather than the lines, which carry no
	// timestamp: a line is what a reader sees, and a rendering does not need one.
	created := make(map[string]state.MemoryItem, len(items))
	for _, it := range items {
		created[it.ID] = it
	}
	sort.SliceStable(cand, func(x, y int) bool {
		a, c := lines[cand[x]], lines[cand[y]]
		pa, pc := usage[a.MemoryID].PushCount, usage[c.MemoryID].PushCount
		if pa != pc {
			return pa < pc
		}
		ta, tc := created[a.MemoryID].CreatedAt, created[c.MemoryID].CreatedAt
		if !ta.Equal(tc) {
			return ta.After(tc)
		}
		return a.MemoryID < c.MemoryID
	})
	return cand
}
