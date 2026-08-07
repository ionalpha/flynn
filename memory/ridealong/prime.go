package ridealong

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/ionalpha/flynn/state"
)

// primeScope is what a run has already been shown unasked. It lives behind a
// context value as a pointer because the set grows during the run: a digest lands
// at wake, another surfacing adds to it an hour later, and a value fixed when the
// run started could only describe what had been pushed before anything happened.
type primeScope struct {
	mu sync.Mutex
	// ids is the set of memory ids pushed at this run's reader. It holds ids rather
	// than items because the only question asked of it is membership, and holding
	// the items would keep a copy of memory alive for the life of the run and let it
	// go stale against the store.
	ids map[string]bool
}

type primeKey struct{}

// NewPrimeScope returns ctx carrying a fresh, empty prime scope: the span within
// which a memory counts as already shown. A host opens one per run, before the wake
// digest is built, so a use recorded late in the run still knows what the reader was
// handed at the start.
//
// Scopes do not nest. One opened inside another is its own and starts empty, which
// is what a caller isolating a subagent wants: the subagent did not see the parent's
// digest, so a fact it recalls is a fact it found. A caller that wants the parent's
// pushes to count passes the parent context down instead of opening a scope.
func NewPrimeScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, primeKey{}, &primeScope{ids: map[string]bool{}})
}

// MarkPushed records that these items were put in front of ctx's reader without
// being asked for. A context with no prime scope is a no-op, so a host that has not
// adopted scopes can call it unconditionally; empty ids are ignored.
//
// Prefer Surfacer.Push, which counts the push in the store and marks the scope
// together. Call this directly only when the store side is already handled, such as
// a host replaying a digest it built earlier in the same run.
func MarkPushed(ctx context.Context, memoryIDs ...string) {
	sc := scopeOf(ctx)
	if sc == nil {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, id := range memoryIDs {
		if id != "" {
			sc.ids[id] = true
		}
	}
}

// Primed reports whether this item has already been pushed at ctx's reader.
func Primed(ctx context.Context, memoryID string) bool {
	sc := scopeOf(ctx)
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.ids[memoryID]
}

// PrimedIDs returns the ids pushed at ctx's reader so far, sorted. It is what an
// audit of a run reads to answer what the session was handed before it went looking
// for anything.
func PrimedIDs(ctx context.Context) []string {
	sc := scopeOf(ctx)
	if sc == nil {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return slices.Sorted(maps.Keys(sc.ids))
}

// OriginFor is how a use of this item should be attributed in ctx: primed if the
// reader had already been shown it, organic otherwise. It is the single definition
// of the split, applied by every read this package makes, and exported so a host
// counting a use through its own path arrives at the same answer rather than
// guessing.
//
// A run with no prime scope reports organic. The alternative reading, that an
// unmarked run might have been primed and should be counted as such, would make
// every use on a host that has not adopted scopes look like a digest effect and
// hide the very signal the split exists to expose. Organic is also the honest
// default in the ordinary case: nothing pushed anything, so the reader found it.
func OriginFor(ctx context.Context, memoryID string) state.UsageOrigin {
	if Primed(ctx, memoryID) {
		return state.UsagePrimed
	}
	return state.UsageOrganic
}

func scopeOf(ctx context.Context) *primeScope {
	sc, _ := ctx.Value(primeKey{}).(*primeScope)
	return sc
}
