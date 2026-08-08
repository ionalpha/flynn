package diag

// The heap counter the detector fits. It is read through runtime/metrics rather than
// MemStats, so a runtime that stopped publishing it is caught here instead of by a
// watchdog silently watching a gap. Before the first collection the value is unknown,
// which is a gap and not a zero, so a process's startup allocation cannot fire anything.

import (
	"math"
	"runtime"
	"testing"
)

// TestHeapLiveBytesIsReadable pins the counter the heap detector fits. It is read
// through runtime/metrics rather than MemStats, so a runtime that stopped
// publishing it must be caught here and not by a watchdog silently watching a gap.
//
// The collection is forced by the test, not by the counter: the metric reports what
// the last collection found.
func TestHeapLiveBytesIsReadable(t *testing.T) {
	runtime.GC()
	if got := heapLiveBytes(); got <= 0 {
		t.Fatalf("heapLiveBytes() = %d; this runtime does not publish %s", got, heapLiveMetric)
	}
}

// TestLiveHeapIsUnknownBeforeTheFirstCollection. Until a collection has run the
// runtime reports a live heap of zero, because it has not yet found out what is
// reachable. Reported as a zero, a process's entire startup heap would appear to
// arrive between two samples, and the watchdog would dump on a program that is
// merely young.
func TestLiveHeapIsUnknownBeforeTheFirstCollection(t *testing.T) {
	if got := liveHeap(0, 0); got != Unknown {
		t.Errorf("liveHeap(0 bytes, 0 collections) = %d, want Unknown", got)
	}
	// A real reading of zero live bytes is still impossible in a Go process, but a
	// completed collection makes the number meaningful, so it is reported as read.
	if got := liveHeap(0, 1); got != 0 {
		t.Errorf("liveHeap(0 bytes, 1 collection) = %d, want 0", got)
	}
	if got := liveHeap(4<<20, 3); got != 4<<20 {
		t.Errorf("liveHeap(4MiB, 3 collections) = %d, want %d", got, 4<<20)
	}
	// The counter is an int64 so Unknown can be told from a size; a heap larger than
	// an int64 saturates rather than wrapping into a negative that reads as Unknown.
	if got := liveHeap(math.MaxUint64, 1); got != math.MaxInt64 {
		t.Errorf("liveHeap(MaxUint64, 1) = %d, want MaxInt64", got)
	}
}

// TestStartupHeapDoesNotFireTheWatchdog is the false positive the counter would have
// produced against a real binary: a process holding a large heap reports a live heap
// of zero until its first collection, and the jump to its true size, followed by
// ordinary growth, is a clean staircase clearing every floor.
//
// Reported as a gap, those samples restart the window instead, so the first slope is
// fitted only across values that mean something.
func TestStartupHeapDoesNotFireTheWatchdog(t *testing.T) {
	w, findings := testWatchdog(t, LeakConfig{
		Window:     4,
		Thresholds: map[string]Threshold{CounterHeapLive: DefaultThresholds()[CounterHeapLive]},
	})

	// Two samples before any collection, then the heap the process was already
	// holding, then a working set that rises and falls as a working set does.
	for _, live := range []int64{Unknown, Unknown, 200 << 20, 260 << 20, 240 << 20, 255 << 20, 245 << 20} {
		w.push(Sample{HeapLiveBytes: live})
	}
	if got := len(*findings); got != 0 {
		t.Fatalf("the watchdog fired %d times on a process that had merely not collected yet", got)
	}
}
