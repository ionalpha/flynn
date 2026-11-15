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

// Defaults for a builder with no options. They are guesses.
const (
	// defaultBudget is in tokens, small enough to afford on every wake, so nobody
	// turns the digest off.
	defaultBudget = 512
	// defaultExplorationQuota reserves budget for rarely pushed items, because a
	// stable order lets the same items win, get used because they are shown, and
	// look deserving.
	defaultExplorationQuota = 0.2
	// defaultSummaryChars fits one terminal line.
	defaultSummaryChars = 160
	// defaultCandidateLimit caps the read, so building cost does not grow with the
	// corpus.
	defaultCandidateLimit = 200
)

// DefaultDemoteAfter is how many unused pushes demote an item. One unused wake is
// noise; five is a verdict. It is exported so a curator report reaches the same
// verdict as the selection.
const DefaultDemoteAfter = 5

// ErrPushNotRecorded reports a push that was not counted. It is returned with the
// digest, never instead, so measurement never fails a wake. It mirrors
// ridealong.ErrUsageNotRecorded.
var ErrPushNotRecorded = errors.New("digest: push not recorded")

// Pusher records items that reached a reader unasked. The default,
// ridealong.Surfacer, counts the push and marks the prime scope together.
type Pusher interface {
	Push(ctx context.Context, memoryIDs []string) error
}

// Builder selects and renders the wake digest. It holds no per-run state; the prime
// scope on the context does.
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

// WithBudget sets the token budget. A non-positive n restores the default. There is
// no unlimited setting, because that is a recall.
func WithBudget(n int) Option {
	return func(b *Builder) {
		if n <= 0 {
			n = defaultBudget
		}
		b.budget = n
	}
}

// WithExplorationQuota sets the budget share for rarely pushed items at the most
// specific scope, clamped to [0,1]. Zero turns exploration off; without another
// diversity policy a fleet converges (see state.Monoculture). An unspent reserve
// returns to the standing order.
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

// WithCandidateLimit caps the items read per selection; hitting it sets
// [Digest.Capped]. A non-positive n restores the default.
func WithCandidateLimit(n int) Option {
	return func(b *Builder) {
		if n <= 0 {
			n = defaultCandidateLimit
		}
		b.candidates = n
	}
}

// WithDemoteAfter sets the unused pushes that demote an item: ranked last and
// skipped by exploration. A non-positive n turns demotion off.
//
// A demoted item still takes leftover budget, and one use clears it
// (state.MemoryUsage.Ignored). Excluding it would be permanent, because an item
// never shown can never be used. Deleting is the curator's call.
func WithDemoteAfter(n int) Option {
	return func(b *Builder) {
		if n < 0 {
			n = 0
		}
		b.demote = n
	}
}

// WithPusher replaces the default [Pusher], so a host with its own does not record
// twice. Nil is ignored.
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
		demote:     DefaultDemoteAfter,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Query is the wake query for scope and its ancestors, most specific first. A zero
// scope spans every scope, so a host that partitions work passes its scope.
func Query(scope state.Scope) state.RecallQuery {
	return state.RecallQuery{Scope: scope, IncludeAncestors: true}
}

// Build selects the digest and records the push, so a later use counts as primed.
// Call it at wake in a context with a prime scope (ridealong.NewPrimeScope). An
// uncounted push returns the digest with [ErrPushNotRecorded].
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

// Select chooses the digest without recording, for previews and tests. A host
// giving it to a session calls Build, or usage under-counts.
//
//  1. Recall q up to the candidate limit and keep what guard.Pushable admits.
//  2. Rank recall order, demoted items last.
//  3. Fill the budget less the reserve in that order.
//  4. Fill the reserve from untaken, undemoted items at the deepest scope.
//  5. Return leftover budget to the standing order.
//
// A line that does not fit is skipped. Lines return in standing order.
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

// usage reads fleet-wide totals (state.TotalUsage) in one call, because an item
// this instance never pushed may be pushed everywhere else. With demotion and
// exploration both off it reads nothing, so an unused read cannot fail the wake.
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
	out := make(map[string]state.MemoryUsage, len(byID))
	for id, rs := range byID {
		out[id] = state.TotalUsage(rs)
	}
	return out, nil
}

// markDemoted flags lines pushed at least the threshold and never used. One use,
// even primed, clears it, because a reader acted on the line. A primed use only
// lifts a demotion and never promotes.
func (b *Builder) markDemoted(lines []Line, usage map[string]state.MemoryUsage) {
	if b.demote == 0 {
		return
	}
	for i := range lines {
		u := usage[lines[i].MemoryID]
		lines[i].Demoted = u.Ignored() && u.PushCount >= int64(b.demote)
	}
}

// standing returns line indices in recall order with demoted lines last.
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

// render drops items with nothing readable.
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

// fill takes lines that fit, in idx order, and returns the remaining budget. Taken
// lines are skipped, so an order can be replayed against a second budget.
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

// exploration returns untaken, undemoted lines at the deepest scope, fewest pushes
// first, then newest, then by id. The deepest scope is where new memory lands with
// no history to compete on. It ranks by pushes because an item never shown has not
// been tried.
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
	// Lines carry no timestamp, so read it from the items.
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
