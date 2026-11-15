package guard

import (
	"context"
	"slices"
	"sync"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// taintScope is a run's taint. It is a pointer in the context because taint is
// found during the run.
type taintScope struct {
	mu      sync.Mutex
	tainted bool
	reasons []string
}

type taintKey struct{}

// NewTaintScope returns ctx with a clean taint scope. Open it before the run reads
// anything, so a late write knows what was read early.
//
// Scopes do not nest: tainting an inner one leaves the outer clean, which isolates a
// subtask. To share taint, pass the outer context down.
func NewTaintScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, taintKey{}, &taintScope{})
}

// Tainted reports untrusted input seen in ctx's scope. With no scope it is false, or
// a host without scopes could push nothing to the digest.
func Tainted(ctx context.Context) bool {
	sc := scopeOf(ctx)
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.tainted
}

// TaintReasons returns the scope's distinct reasons in marking order, for auditing
// why a fact was kept out of the digest.
func TaintReasons(ctx context.Context) []string {
	sc := scopeOf(ctx)
	if sc == nil {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return slices.Clone(sc.reasons)
}

// MarkTainted records untrusted input with a reason such as "tool:shell". Nothing
// clears a scope; a repeated reason is kept once. Without a scope it does nothing.
func MarkTainted(ctx context.Context, reason string) {
	sc := scopeOf(ctx)
	if sc == nil {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.tainted = true
	if reason != "" && !slices.Contains(sc.reasons, reason) {
		sc.reasons = append(sc.reasons, reason)
	}
}

// Observe marks the scope for each untrusted source. A host calls it where content
// enters, because a write's claimed sources can omit what the run read. It reports
// whether the scope is now tainted, including from earlier calls.
func Observe(ctx context.Context, sources ...string) bool {
	for _, s := range sources {
		if TrustOf(s) == sandbox.TrustUntrusted {
			MarkTainted(ctx, s)
		}
	}
	return Tainted(ctx)
}

// TaintItem sets Tainted if the item's provenance is untrusted or ctx is tainted,
// and never clears it. [Store.Write] applies it; it is exported so a host writing
// elsewhere agrees. Provenance is folded in at write, so a later rewrite of the
// sources cannot shed the taint.
func TaintItem(ctx context.Context, it state.MemoryItem) state.MemoryItem {
	if it.Tainted {
		return it
	}
	it.Tainted = TrustOfAll(it.Sources) == sandbox.TrustUntrusted || Tainted(ctx)
	return it
}

func scopeOf(ctx context.Context) *taintScope {
	sc, _ := ctx.Value(taintKey{}).(*taintScope)
	return sc
}
