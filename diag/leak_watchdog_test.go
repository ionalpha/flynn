package diag

// The watchdog over a running timeline: what it does with the samples the detector
// judges. A gap is never bridged, because a slope fitted across a platform's silence is
// fiction. A counter fires once, and again only after a whole fresh window, because an
// unattended process must not fill a disk with evidence of a fact it already recorded.

import "testing"

// TestAppendCappedKeepsTheNewestAndDoesNotGrow: the window rides a sampler that
// runs for the life of the process, so a window that reallocates every sample is a
// leak in the leak detector.
func TestAppendCappedKeepsTheNewestAndDoesNotGrow(t *testing.T) {
	var xs []float64
	for i := range 100 {
		xs = appendCapped(xs, float64(i), 4)
	}
	if want := []float64{96, 97, 98, 99}; !equalFloats(xs, want) {
		t.Errorf("window = %v, want %v", xs, want)
	}
	if cap(xs) > 8 {
		t.Errorf("window capacity grew to %d over 100 samples", cap(xs))
	}
}

// TestCounterValueReportsAGapNotAZero: a platform that cannot measure a counter
// must not be read as measuring zero, or the detector fits a slope across the
// platform's silence and dumps a profile of nothing.
func TestCounterValueReportsAGapNotAZero(t *testing.T) {
	unknown := Sample{
		Goroutines:    12,
		HeapLiveBytes: Unknown,
		OpenFDs:       Unknown,
		ChildProcs:    Unknown,
		Extra:         map[string]float64{"present": 3, "absent": Unknown},
	}
	for _, name := range []string{CounterHeapLive, CounterOpenFDs, CounterChildProcs, "absent", "never_registered"} {
		if v, ok := counterValue(unknown, name); ok {
			t.Errorf("counterValue(%s) = %v, true; want a gap", name, v)
		}
	}
	if v, ok := counterValue(unknown, CounterGoroutines); !ok || v != 12 {
		t.Errorf("counterValue(goroutines) = %v, %v; want 12, true", v, ok)
	}
	if v, ok := counterValue(unknown, "present"); !ok || v != 3 {
		t.Errorf("counterValue(present) = %v, %v; want 3, true", v, ok)
	}
}

// TestWatchdogGapRestartsTheWindow: an unmeasurable sample in the middle of a ramp
// must not be bridged. A slope fitted across a gap is fiction.
func TestWatchdogGapRestartsTheWindow(t *testing.T) {
	w, findings := testWatchdog(t, LeakConfig{
		Window:     4,
		Thresholds: map[string]Threshold{CounterOpenFDs: {MinSlope: 0.5, MinDelta: 2}},
	})

	// A clean ramp, but the third sample could not be measured.
	for _, fd := range []int{10, 20, Unknown, 30, 40, 50} {
		w.push(Sample{OpenFDs: fd})
	}
	if got := len(*findings); got != 0 {
		t.Fatalf("fired %d times across a gap; the window must restart at one", got)
	}

	// Three more clean samples complete a fresh window, and now it fires.
	w.push(Sample{OpenFDs: 60})
	if got := len(*findings); got != 1 {
		t.Fatalf("fired %d times after the window refilled, want 1", got)
	}
}

// TestWatchdogFiresOncePerCounter: an unattended process must not fill a disk with
// evidence of a fact it already recorded.
func TestWatchdogFiresOncePerCounter(t *testing.T) {
	w, findings := testWatchdog(t, LeakConfig{
		Window:     4,
		Thresholds: map[string]Threshold{CounterGoroutines: {MinSlope: 1, MinDelta: 4}},
	})

	for i := range 40 {
		w.push(Sample{Goroutines: 10 + i*10})
	}
	if got := len(*findings); got != 1 {
		t.Fatalf("fired %d times on a run-long ramp, want exactly 1", got)
	}
	if f := (*findings)[0]; f.Counter != CounterGoroutines {
		t.Errorf("finding names counter %q, want %q", f.Counter, CounterGoroutines)
	}
}

// TestWatchdogRepeatNeedsAFreshWindow: with Repeat on, a leak that is still leaking
// fires again, but only after a whole new window of sustained growth. Without the
// reset it would fire on every subsequent sample.
func TestWatchdogRepeatNeedsAFreshWindow(t *testing.T) {
	w, findings := testWatchdog(t, LeakConfig{
		Window:     4,
		Repeat:     true,
		Thresholds: map[string]Threshold{CounterGoroutines: {MinSlope: 1, MinDelta: 4}},
	})

	const samples = 40
	for i := range samples {
		w.push(Sample{Goroutines: 10 + i*10})
	}
	// One firing per full window, not one per sample.
	if got, want := len(*findings), samples/4; got != want {
		t.Fatalf("fired %d times over %d samples with a window of 4, want %d", got, samples, want)
	}
}

// TestWatchdogWatchesOnlyCountersWithThresholds: a Thresholds map is a statement of
// what to watch. A counter absent from it is recorded in the timeline and never
// fires, which is how a caller watches goroutines alone.
func TestWatchdogWatchesOnlyCountersWithThresholds(t *testing.T) {
	w, findings := testWatchdog(t, LeakConfig{
		Window:     4,
		Thresholds: map[string]Threshold{CounterGoroutines: {MinSlope: 1, MinDelta: 4}},
	})

	// Descriptors ramp hard; goroutines stay flat. Only fds were left unwatched.
	for i := range 20 {
		w.push(Sample{Goroutines: 10, OpenFDs: 10 + i*10})
	}
	if got := len(*findings); got != 0 {
		t.Fatalf("fired %d times on a counter with no threshold", got)
	}
}

// TestWatchdogFindingCarriesTheWholeSampleNotJustTheCounter: the counter that fired
// names the leak class, and the rest of the line is what an operator reads next.
func TestWatchdogFindingCarriesTheWholeSampleNotJustTheCounter(t *testing.T) {
	w, findings := testWatchdog(t, LeakConfig{
		Window:     4,
		Thresholds: map[string]Threshold{CounterGoroutines: {MinSlope: 1, MinDelta: 4}},
	})

	for i := range 4 {
		w.push(Sample{Goroutines: 10 + i*10, HeapAllocBytes: uint64(1000 + i), OpenFDs: 7})
	}
	if len(*findings) != 1 {
		t.Fatalf("want exactly one finding, got %d", len(*findings))
	}
	f := (*findings)[0]
	if len(f.Window) != 4 {
		t.Fatalf("finding carries %d samples, want the whole window of 4", len(f.Window))
	}
	if f.Window[0].OpenFDs != 7 || f.Window[3].HeapAllocBytes != 1003 {
		t.Errorf("finding's window lost the rest of the sample: %+v", f.Window)
	}
	if f.First != 10 || f.Last != 40 || f.Delta != 30 {
		t.Errorf("finding = first %v last %v delta %v, want 10 40 30", f.First, f.Last, f.Delta)
	}
}
