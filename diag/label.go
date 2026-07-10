package diag

import (
	"context"
	"runtime/pprof"
	"sync/atomic"
)

// profiling reports whether a bundle is open. It is the guard on every labelling
// path: a process that is not being profiled must not pay for labels it will
// never write out. Start sets it, Stop clears it.
var profiling atomic.Bool

// Profiling reports whether a bundle is currently open in this process. Call it
// before building label values that cost anything to produce; the labelling
// helpers below check it themselves, so a caller with labels already in hand
// does not need to.
func Profiling() bool { return profiling.Load() }

// Labeled runs fn with kv attached as pprof labels to the calling goroutine and,
// by inheritance, to every goroutine fn starts. The labels are removed when fn
// returns.
//
// This is what turns a wall of identical stacks into an attribution: a goroutine
// profile of a leaking agent shows several thousand goroutines parked in a
// channel receive, and nothing in the stack says which action left them there.
// Labelled, the same profile reads as 1,900 goroutines under one action, and the
// leak names itself.
//
// When no bundle is open, fn is called directly: no label map, no context value,
// no allocation. kv is a flat key, value, key, value list; a trailing key with no
// value is dropped rather than fatal, because a mislabelled profile is a smaller
// problem than a killed run.
func Labeled(ctx context.Context, fn func(context.Context), kv ...string) {
	if !profiling.Load() {
		fn(ctx)
		return
	}
	pprof.Do(ctx, pprof.Labels(pairs(kv)...), fn)
}

// LabelGoroutine attaches kv to the calling goroutine for the rest of its life,
// and returns a context carrying the same labels so goroutines it starts inherit
// them. Call it as the first statement of a long-lived goroutine: a subscription
// pump, a queue worker, a resync loop. Unlike Labeled there is nothing to unwind,
// because the goroutine's whole lifetime is the scope.
//
// When no bundle is open it returns ctx unchanged and does nothing.
func LabelGoroutine(ctx context.Context, kv ...string) context.Context {
	if !profiling.Load() {
		return ctx
	}
	ctx = pprof.WithLabels(ctx, pprof.Labels(pairs(kv)...))
	pprof.SetGoroutineLabels(ctx)
	return ctx
}

// pairs trims a trailing unpaired key. pprof.Labels panics on an odd argument
// count, and no diagnostic aid is worth a panic in a governed run.
func pairs(kv []string) []string {
	if len(kv)%2 != 0 {
		return kv[:len(kv)-1]
	}
	return kv
}
