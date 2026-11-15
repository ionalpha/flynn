package diag

import (
	"context"
	"runtime/pprof"
	"sync/atomic"
)

// profiling reports whether a bundle is open; every labelling path checks it so an
// unprofiled process pays nothing. Start sets it and Stop clears it.
var profiling atomic.Bool

// Profiling reports whether a bundle is open in this process. Check it before
// building costly label values; the labelling helpers check it themselves.
func Profiling() bool { return profiling.Load() }

// Labeled runs fn with kv attached as pprof labels to the calling goroutine and,
// by inheritance, every goroutine fn starts; the labels are removed when fn
// returns. Labels turn a wall of identical parked stacks into "1,900 goroutines
// under one action".
//
// With no bundle open, fn is called directly with no allocation. kv is a flat
// key, value list; a trailing unpaired key is dropped, because a mislabelled
// profile costs less than a killed run.
func Labeled(ctx context.Context, fn func(context.Context), kv ...string) {
	if !profiling.Load() {
		fn(ctx)
		return
	}
	pprof.Do(ctx, pprof.Labels(pairs(kv)...), fn)
}

// LabelGoroutine attaches kv to the calling goroutine for the rest of its life and
// returns a context carrying the same labels, so goroutines it starts inherit them.
// Call it first in a long-lived goroutine such as a queue worker. With no bundle
// open it returns ctx unchanged.
func LabelGoroutine(ctx context.Context, kv ...string) context.Context {
	if !profiling.Load() {
		return ctx
	}
	ctx = pprof.WithLabels(ctx, pprof.Labels(pairs(kv)...))
	pprof.SetGoroutineLabels(ctx)
	return ctx
}

// pairs trims a trailing unpaired key, because pprof.Labels panics on an odd count.
func pairs(kv []string) []string {
	if len(kv)%2 != 0 {
		return kv[:len(kv)-1]
	}
	return kv
}
