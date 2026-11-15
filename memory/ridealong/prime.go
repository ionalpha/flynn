package ridealong

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/ionalpha/flynn/state"
)

// primeScope is what a run was shown unasked. It is a pointer in the context
// because the set grows during the run.
type primeScope struct {
	mu sync.Mutex
	// ids holds ids only, because membership is the only question and items would
	// go stale.
	ids map[string]bool
}

type primeKey struct{}

// NewPrimeScope returns ctx with an empty prime scope, the span in which a memory
// counts as shown. Open one per run before building the digest.
//
// Scopes do not nest: an inner one starts empty, because a subagent did not see the
// parent's digest. To share the parent's, pass its context down.
func NewPrimeScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, primeKey{}, &primeScope{ids: map[string]bool{}})
}

// MarkPushed marks items as shown unasked. Without a prime scope it does nothing;
// empty ids are ignored. Prefer [Surfacer.Push], which also counts the push; call
// this only when the store side is done, such as replaying a digest.
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

// Primed reports whether the item was pushed in ctx's scope.
func Primed(ctx context.Context, memoryID string) bool {
	sc := scopeOf(ctx)
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.ids[memoryID]
}

// PrimedIDs returns the ids pushed so far, sorted, for auditing a run.
func PrimedIDs(ctx context.Context) []string {
	sc := scopeOf(ctx)
	if sc == nil {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return slices.Sorted(maps.Keys(sc.ids))
}

// OriginFor attributes a use: primed if the item was shown, organic otherwise. It is
// exported so a host counting uses elsewhere agrees. With no prime scope it reports
// organic, or every use on a host without scopes would look like a digest effect.
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
