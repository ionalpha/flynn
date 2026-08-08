package diag

// What reaches disk when a counter fires. An unattended dump has to stand on its own at
// 3am: the profiles, the window that fired, a log record naming the counter and the
// slope, and a manifest that hashes the dump like any other member. A bundle with no
// watchdog carries none of it, and a failed write leaves no partial behind.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

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
