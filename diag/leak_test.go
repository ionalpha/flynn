package diag

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/observe"
)

// ramp is a Threshold loose enough that any of the shapes below could clear the
// floors, so what the shape tests actually exercise is the separation rule rather
// than the floors. The floor tests set their own.
var ramp = Threshold{MinSlope: 0.5, MinDelta: 4}

// TestDetectFiresOnSustainedGrowth is the case the watchdog exists for: a counter
// that rises across the whole window and never once comes back.
func TestDetectFiresOnSustainedGrowth(t *testing.T) {
	vs := []float64{10, 12, 14, 16, 18, 20, 22, 24, 26, 28, 30, 32}

	f, ok := detect(vs, ramp)
	if !ok {
		t.Fatalf("detect(%v) did not fire on a clean ramp", vs)
	}
	if f.Slope != 2 {
		t.Errorf("slope = %v, want 2", f.Slope)
	}
	if f.Delta != 22 {
		t.Errorf("delta = %v, want 22", f.Delta)
	}
	if f.First != 10 || f.Last != 32 {
		t.Errorf("first,last = %v,%v, want 10,32", f.First, f.Last)
	}
}

// TestDetectDoesNotFireOnNormalShapes is the test that decides whether anyone
// leaves --leak-watch on. Every shape here rises somewhere, and a naive "slope > 0"
// detector calls several of them leaks. None of them is one.
func TestDetectDoesNotFireOnNormalShapes(t *testing.T) {
	cases := []struct {
		name string
		vs   []float64
		why  string
	}{
		{
			name: "flat",
			vs:   []float64{40, 40, 40, 40, 40, 40, 40, 40},
			why:  "a steady process",
		},
		{
			name: "sawtooth heap between collections",
			vs:   []float64{10, 30, 50, 12, 32, 52, 14, 34, 54, 16, 36, 56},
			why:  "the live heap rises and falls with the collector; the trend is up, the retention is not",
		},
		{
			name: "single spike",
			vs:   []float64{10, 10, 10, 10, 10, 90, 10, 10, 10, 10, 10, 10},
			why:  "one expensive turn, fully released",
		},
		{
			name: "step that settles",
			vs:   []float64{10, 10, 10, 10, 10, 10, 60, 60, 60, 60, 60, 60},
			why:  "a fan-out raised the floor once and held it; a step is not a slope",
		},
		{
			name: "fan-out that returns to baseline",
			vs:   []float64{20, 45, 70, 95, 120, 140, 120, 95, 70, 45, 22, 20},
			why:  "child agents spawned and were reaped",
		},
		{
			name: "noisy but level",
			vs:   []float64{50, 47, 53, 49, 51, 48, 52, 50, 47, 53, 49, 51},
			why:  "jitter without trend",
		},
		{
			name: "ramp with one dip",
			vs:   []float64{10, 12, 14, 16, 18, 20, 22, 11, 26, 28, 30, 32},
			why:  "growth that reverses even once is not yet sustained; the next window will catch it if it is real",
		},
		{
			name: "monotone decline",
			vs:   []float64{90, 80, 70, 60, 50, 40, 30, 20},
			why:  "a cache draining",
		},
		{
			name: "short window",
			vs:   []float64{1, 2, 3},
			why:  "three points admit any slope",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if f, ok := detect(tc.vs, ramp); ok {
				t.Errorf("detect fired on %v (slope %v, delta %v): %s", tc.vs, f.Slope, f.Delta, tc.why)
			}
		})
	}
}

// TestDetectHonoursBothFloors: separation alone is not enough. Growth too slow to
// matter over the life of a process, and growth too small to matter at all, are
// both rejected even though each rises monotonically.
func TestDetectHonoursBothFloors(t *testing.T) {
	// Rises by 1 per sample, cleanly separated: 11 over the window.
	slow := []float64{100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111}

	if _, ok := detect(slow, Threshold{MinSlope: 10, MinDelta: 4}); ok {
		t.Error("detect fired on a slope of 1 against a floor of 10")
	}
	if _, ok := detect(slow, Threshold{MinSlope: 0.5, MinDelta: 500}); ok {
		t.Error("detect fired on a delta of 11 against a floor of 500")
	}
	if _, ok := detect(slow, Threshold{MinSlope: 0.5, MinDelta: 4}); !ok {
		t.Error("detect did not fire on a slope of 1 and a delta of 11 against floors it clears")
	}
}

// TestLeastSquaresSlope pins the fit itself, so a refactor of the one-pass form
// cannot quietly change what every threshold is measured against.
func TestLeastSquaresSlope(t *testing.T) {
	cases := []struct {
		vs   []float64
		want float64
	}{
		{[]float64{0, 1, 2, 3}, 1},
		{[]float64{0, 2, 4, 6}, 2},
		{[]float64{5, 5, 5, 5}, 0},
		{[]float64{3, 2, 1, 0}, -1},
		// A least-squares fit is not the endpoint difference: the dip pulls it down.
		{[]float64{0, 10, 0, 10}, 2},
	}
	for _, tc := range cases {
		if got := leastSquaresSlope(tc.vs); got != tc.want {
			t.Errorf("leastSquaresSlope(%v) = %v, want %v", tc.vs, got, tc.want)
		}
	}
	if got := leastSquaresSlope([]float64{7}); got != 0 {
		t.Errorf("leastSquaresSlope of one point = %v, want 0", got)
	}
}

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

// TestNewWatchdogRejectsAConfigThatWouldFireOnNoise. Both rejections are at Start,
// not at the first sample: an operator learns their soak was misconfigured before
// they leave it running for a week.
func TestNewWatchdogRejectsAConfigThatWouldFireOnNoise(t *testing.T) {
	nodump := func(Finding) ([]string, error) { return nil, nil }

	if _, err := newWatchdog(LeakConfig{Window: 3}, nodump); err == nil {
		t.Error("a window of 3 was accepted; three points admit any slope")
	}
	for _, th := range []Threshold{{MinSlope: 0, MinDelta: 4}, {MinSlope: 1, MinDelta: 0}, {MinSlope: -1, MinDelta: -1}} {
		cfg := LeakConfig{Thresholds: map[string]Threshold{CounterGoroutines: th}}
		if _, err := newWatchdog(cfg, nodump); err == nil {
			t.Errorf("threshold %+v was accepted; a zero floor fires on noise", th)
		}
	}
	if _, err := newWatchdog(LeakConfig{}, nodump); err != nil {
		t.Errorf("the default config was rejected: %v", err)
	}
}

// TestStartRejectsAWatchdogWithNoSampler: the watchdog rides the timeline. Without
// one it would watch nothing, silently, which is the one failure an operator would
// never notice.
func TestStartRejectsAWatchdogWithNoSampler(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")

	b, err := Start(Config{Dir: dir, Interval: -1, Clock: clock.NewManual(epoch), Leak: &LeakConfig{}})
	if err == nil {
		_ = b.Stop()
		t.Fatal("Start accepted a leak watch with the sampler disabled")
	}
	if !strings.Contains(err.Error(), "sampler") {
		t.Errorf("error %q does not say the sampler is missing", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a rejected config created the bundle directory")
	}
}

// TestStartRejectsABadThresholdBeforeTouchingTheBundle. The same contract as above:
// nothing is opened, so no CPU profile runs and no half-written bundle is left for
// a reader to trust.
func TestStartRejectsABadThresholdBeforeTouchingTheBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	cfg := Config{
		Dir:      dir,
		Interval: time.Second,
		Clock:    clock.NewManual(epoch),
		Leak:     &LeakConfig{Thresholds: map[string]Threshold{CounterGoroutines: {}}},
	}

	if b, err := Start(cfg); err == nil {
		_ = b.Stop()
		t.Fatal("Start accepted a zero threshold")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a rejected config created the bundle directory")
	}
}

// TestStartRejectsACounterWithNoRead. A Counter with a nil Read would be called on
// the baseline sample and panic before Start could return. Reject it at Start, with an
// error naming the counter, and touch no bundle.
func TestStartRejectsACounterWithNoRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	cfg := Config{
		Dir:      dir,
		Interval: time.Second,
		Clock:    clock.NewManual(epoch),
		Counters: []Counter{{Name: "queued"}}, // Read is nil
	}

	b, err := Start(cfg)
	if err == nil {
		_ = b.Stop()
		t.Fatal("Start accepted a counter with no Read function")
	}
	if !strings.Contains(err.Error(), "queued") {
		t.Errorf("error %q does not name the offending counter", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a rejected config created the bundle directory")
	}
}

// TestStartRejectsACounterNameThatEscapesTheBundle. A firing counter's name is spliced
// into a leak dump filename, so a name with a path separator or ".." would write
// outside the bundle or into a directory that does not exist, and the evidence would be
// silently lost. Reject such a name at Start.
func TestStartRejectsACounterNameThatEscapesTheBundle(t *testing.T) {
	for _, name := range []string{"cache/entries", `a\b`, "..", ".", ""} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bundle")
			cfg := Config{
				Dir:      dir,
				Interval: time.Second,
				Clock:    clock.NewManual(epoch),
				Counters: []Counter{{Name: name, Read: func() float64 { return 0 }}},
			}
			if b, err := Start(cfg); err == nil {
				_ = b.Stop()
				t.Fatalf("Start accepted the unsafe counter name %q", name)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Error("a rejected config created the bundle directory")
			}
		})
	}
}

// TestSafeCounterNameAcceptsPlainNamesAndRejectsPaths pins the rule directly: a plain
// name is a valid filename component, and anything that could steer a dump out of the
// bundle is not.
func TestSafeCounterNameAcceptsPlainNamesAndRejectsPaths(t *testing.T) {
	for _, ok := range []string{"queued", "temp_dirs", "event.log.bytes", CounterGoroutines} {
		if !safeCounterName(ok) {
			t.Errorf("safeCounterName(%q) = false, want a plain name accepted", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../escape", "dir/leak"} {
		if safeCounterName(bad) {
			t.Errorf("safeCounterName(%q) = true, want an unsafe name rejected", bad)
		}
	}
}

// TestFromEnvEnablesTheWatchdog: a hosted instance whose command line an operator
// cannot change is still watchable.
func TestFromEnvEnablesTheWatchdog(t *testing.T) {
	t.Setenv(EnvLeakWatch, "1")
	if cfg := FromEnv(Config{}); cfg.Leak == nil {
		t.Error("FLYNN_LEAK_WATCH=1 did not enable the watchdog")
	}

	t.Setenv(EnvLeakWatch, "0")
	if cfg := FromEnv(Config{}); cfg.Leak != nil {
		t.Error("FLYNN_LEAK_WATCH=0 enabled the watchdog")
	}

	// An explicit config wins over the environment, and a false environment value
	// never disables a watchdog the caller asked for.
	explicit := &LeakConfig{Repeat: true}
	if cfg := FromEnv(Config{Leak: explicit}); cfg.Leak != explicit {
		t.Error("the environment overrode an explicit LeakConfig")
	}

	t.Setenv(EnvLeakWatch, "not-a-bool")
	if cfg := FromEnv(Config{}); cfg.Leak != nil {
		t.Error("an unparseable FLYNN_LEAK_WATCH enabled the watchdog")
	}
}

// TestBundleDumpsEvidenceWhenACounterLeaks drives the whole path a real leak takes:
// a bundle with a watchdog, a counter that ramps, a Manual clock stepped one sample
// at a time. It asserts the four things an operator needs from an unattended dump:
// the profiles are on disk, the window that fired is with them, the log record names
// the counter and the slope, and the sealed manifest hashes the dump like any other
// member, so evidence collected at 3am can still be shown to be intact.
func TestBundleDumpsEvidenceWhenACounterLeaks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{})
	log := &captureLogger{}

	// A synthetic counter under the watchdog's own contract: the built-in counters
	// describe this test process, which must not be made to leak to test the detector.
	var reads int
	leaky := Counter{Name: "leaky", Read: func() float64 {
		reads++
		return float64(reads * 100)
	}}

	b, err := Start(Config{
		Dir:      dir,
		Interval: time.Second,
		Clock:    clk,
		Counters: []Counter{leaky},
		Leak: &LeakConfig{
			Window:     4,
			Logger:     log,
			Thresholds: map[string]Threshold{"leaky": {MinSlope: 10, MinDelta: 100}},
		},
		sampled: sampled,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start wrote the baseline sample, so three more ticks complete the first full
	// window of four. The fourth tick cannot fire again: a counter fires once.
	for range 4 {
		clk.Advance(time.Second)
		<-sampled
	}

	if got := log.warnings(); got != 1 {
		t.Fatalf("watchdog logged %d warnings over a clean ramp, want 1", got)
	}
	rec := log.records[0]
	if !strings.Contains(rec.msg, "leak") {
		t.Errorf("log record %q does not mention a leak", rec.msg)
	}
	if got := rec.field("counter"); got != "leaky" {
		t.Errorf("log record names counter %v, want leaky", got)
	}
	if got, ok := rec.field("slope_per_sample").(float64); !ok || got != 100 {
		t.Errorf("log record reports slope %v, want 100", rec.field("slope_per_sample"))
	}

	// The dump is on disk before the bundle is sealed, so a process that is killed
	// after the dump still leaves the evidence behind.
	dumps := []string{
		"leak.leaky.0.goroutine.labels.txt",
		"leak.leaky.0.goroutine.txt",
		"leak.leaky.0.heap.pprof",
		"leak.leaky.0.window.jsonl",
	}
	for _, name := range dumps {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("dump member %s missing: %v", name, err)
		}
	}
	if entries, err := filepath.Glob(filepath.Join(dir, "*.partial")); err != nil || len(entries) != 0 {
		t.Errorf("a committed dump left temporary files behind: %v", entries)
	}

	// The window is the samples that fired, oldest first, in the timeline's own shape.
	window := readSamples(t, filepath.Join(dir, "leak.leaky.0.window.jsonl"))
	if len(window) != 4 {
		t.Fatalf("window has %d samples, want the 4 the detector fitted", len(window))
	}
	// The window is the first four samples: the baseline and the three ticks that
	// completed it, which is the moment the growth became sustained.
	for i, s := range window {
		if want := float64((i + 1) * 100); s.Extra["leaky"] != want {
			t.Errorf("window sample %d carries leaky=%v, want %v", i, s.Extra["leaky"], want)
		}
	}
	if !window[0].T.Before(window[3].T) {
		t.Error("window is not in sample order")
	}

	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The manifest hashes the dump like any other member: a bundle carrying evidence
	// of a leak is still a bundle whose integrity can be shown.
	m := readManifest(t, dir)
	for _, name := range dumps {
		if !manifestHasMember(m, name) {
			t.Errorf("manifest does not list dump member %s", name)
		}
	}
}

// TestBundleWithoutAWatchdogDumpsNothing: the sampler behaves exactly as it does
// without the watchdog, and a bundle carries no leak members it was not asked for.
func TestBundleWithoutAWatchdogDumpsNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{})

	// The same ramp that fired above, with no LeakConfig to watch it.
	var reads int
	leaky := Counter{Name: "leaky", Read: func() float64 { reads++; return float64(reads * 100) }}

	b, err := Start(Config{Dir: dir, Interval: time.Second, Clock: clk, Counters: []Counter{leaky}, sampled: sampled})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 6 {
		clk.Advance(time.Second)
		<-sampled
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	entries, err := filepath.Glob(filepath.Join(dir, "leak.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a bundle with no watchdog wrote %v", entries)
	}

	// The counter still reaches the timeline: recording and watching are separate.
	samples := readSamples(t, filepath.Join(dir, MemberTimeline))
	if len(samples) == 0 || samples[0].Extra["leaky"] == 0 {
		t.Error("an application counter did not reach the timeline")
	}
}

// TestSamplerFinalSampleIsNotWatched. A leak reported as the process exits tells an
// operator nothing the bundle's exit-time profiles do not already say, and dumping
// one would race the manifest that is about to hash the directory.
func TestSamplerFinalSampleIsNotWatched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{})
	log := &captureLogger{}

	var reads int
	leaky := Counter{Name: "leaky", Read: func() float64 { reads++; return float64(reads * 100) }}

	// A window of 5 with a baseline and 3 ticks leaves the detector one sample short:
	// only Stop's final sample could complete it, and Stop must not offer it.
	b, err := Start(Config{
		Dir: dir, Interval: time.Second, Clock: clk, Counters: []Counter{leaky}, sampled: sampled,
		Leak: &LeakConfig{Window: 5, Logger: log, Thresholds: map[string]Threshold{"leaky": {MinSlope: 10, MinDelta: 100}}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 3 {
		clk.Advance(time.Second)
		<-sampled
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := log.warnings(); got != 0 {
		t.Errorf("the final sample completed a window and fired %d times", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "leak.*")); len(entries) != 0 {
		t.Errorf("Stop's final sample dumped %v", entries)
	}
}

// TestLeakDumpsAreNumbered: with Repeat on, the tenth firing must not overwrite the
// first, which is the interesting one.
func TestLeakDumpsAreNumbered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{})

	var reads int
	leaky := Counter{Name: "leaky", Read: func() float64 { reads++; return float64(reads * 100) }}

	b, err := Start(Config{
		Dir: dir, Interval: time.Second, Clock: clk, Counters: []Counter{leaky}, sampled: sampled,
		Leak: &LeakConfig{Window: 4, Repeat: true, Thresholds: map[string]Threshold{"leaky": {MinSlope: 10, MinDelta: 100}}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 8 { // baseline plus 8 ticks: two full windows of four
		clk.Advance(time.Second)
		<-sampled
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	for _, seq := range []string{"0", "1"} {
		name := filepath.Join(dir, "leak.leaky."+seq+".window.jsonl")
		if _, err := os.Stat(name); err != nil {
			t.Errorf("firing %s left no window: %v", seq, err)
		}
	}
	first := readSamples(t, filepath.Join(dir, "leak.leaky.0.window.jsonl"))
	second := readSamples(t, filepath.Join(dir, "leak.leaky.1.window.jsonl"))
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("a numbered dump is empty")
	}
	if !first[0].T.Before(second[0].T) {
		t.Error("the second dump overwrote the first")
	}
}

// TestWriteAtomicLeavesNoPartialOnFailure: a reader watching the bundle directory
// never opens a profile that is still being written, and a failed write leaves no
// carcass for the manifest to hash.
func TestWriteAtomicLeavesNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	b := &Bundle{cfg: Config{Dir: dir}}

	sentinel := errors.New("write failed")
	if err := b.writeAtomic("member.txt", func(*os.File) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("writeAtomic error = %v, want it to wrap %v", err, sentinel)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed write left %d files behind", len(entries))
	}

	if err := b.writeAtomic("member.txt", func(f *os.File) error { _, err := f.WriteString("ok"); return err }); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "member.txt")); err != nil || string(data) != "ok" {
		t.Errorf("member = %q, %v; want %q", data, err, "ok")
	}
}

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

// --- helpers -----------------------------------------------------------------

// testWatchdog builds a watchdog whose dumps are recorded rather than written, so
// the detector's behaviour is tested without a bundle on disk.
func testWatchdog(t *testing.T, cfg LeakConfig) (*watchdog, *[]Finding) {
	t.Helper()
	findings := &[]Finding{}
	w, err := newWatchdog(cfg, func(f Finding) ([]string, error) {
		*findings = append(*findings, f)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("newWatchdog: %v", err)
	}
	return w, findings
}

// captureLogger records what the watchdog reported, so a test asserts on the record
// an operator would actually read.
type captureLogger struct {
	observe.NopLogger
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	msg    string
	fields []observe.Field
}

func (r logRecord) field(key string) any {
	for _, f := range r.fields {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func (l *captureLogger) Warn(_ context.Context, msg string, fields ...observe.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, logRecord{msg: msg, fields: fields})
}

func (l *captureLogger) warnings() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

// readSamples decodes a JSONL member as the timeline's own Sample type.
func readSamples(t *testing.T, path string) []Sample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []Sample
	dec := json.NewDecoder(f)
	for dec.More() {
		var s Sample
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		out = append(out, s)
	}
	return out
}

func manifestHasMember(m Manifest, name string) bool {
	for _, mem := range m.Members {
		if mem.Name == name {
			return mem.Bytes > 0 && mem.SHA256 != ""
		}
	}
	return false
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
