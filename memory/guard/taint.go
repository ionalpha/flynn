package guard

import (
	"context"
	"slices"
	"sync"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// taintScope is the mutable taint state a run carries. It lives behind a context
// value as a pointer because taint is discovered during the run - a tool returns
// a page halfway through - and a context value fixed at the top of the run could
// only record what was already known when it started.
type taintScope struct {
	mu      sync.Mutex
	tainted bool
	reasons []string
}

type taintKey struct{}

// NewTaintScope returns ctx carrying a fresh, untainted taint scope: the span
// within which untrusted input is remembered. A run opens one before it reads
// anything, so a write late in the run still knows what the run consumed early.
//
// Scopes do not nest into one another. A scope opened inside another is its own,
// and marking the inner one leaves the outer clean, which is what a caller
// deliberately isolating a subtask wants. A caller that wants the taint to reach
// the outer run passes the outer context down rather than opening a new scope.
func NewTaintScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, taintKey{}, &taintScope{})
}

// Tainted reports whether untrusted input has been observed in ctx's scope. A
// context with no scope reports false: an unmarked run is not evidence of
// laundering, and answering true would make every write of a host that has not
// adopted taint scopes ineligible for the wake digest.
func Tainted(ctx context.Context) bool {
	sc := scopeOf(ctx)
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.tainted
}

// TaintReasons returns the reasons recorded on ctx's scope, in the order they were
// marked, deduplicated. It is what an audit reads to answer why a fact was kept out
// of the digest; a clean or scopeless context returns nothing.
func TaintReasons(ctx context.Context) []string {
	sc := scopeOf(ctx)
	if sc == nil {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return slices.Clone(sc.reasons)
}

// MarkTainted records that untrusted input entered ctx's scope, with a short reason
// for the audit trail ("tool:shell", "web:fetch"). It is idempotent per reason and
// monotone: nothing un-taints a scope. A context with no scope is a no-op, so a
// host can call it unconditionally.
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

// Observe records the arrival of content from sources into ctx's scope, marking it
// tainted for each untrusted one. It is the call a host puts on its ingest path -
// a tool result, an inbound message, a fetched page - so taint is recorded where
// the content enters rather than reconstructed later from what a write claims.
//
// It reports whether the scope is tainted afterwards, including by an earlier
// observation, so a caller can branch on it without a second call.
func Observe(ctx context.Context, sources ...string) bool {
	for _, s := range sources {
		if TrustOf(s) == sandbox.TrustUntrusted {
			MarkTainted(ctx, s)
		}
	}
	return Tainted(ctx)
}

// TaintItem returns it with Tainted set if the item's own provenance is untrusted
// or ctx's scope is tainted, leaving an already-tainted item tainted. It is the
// single definition of what taints a write, applied by Store.Write, and exported so
// a host writing through its own path arrives at the same answer.
//
// Provenance is folded in here rather than left to the eligibility read because the
// two answer different questions at different times: what the run consumed is known
// only at the write, and an item whose sources are rewritten by a later consolidation
// pass must not be able to shed a taint it earned.
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
