package diag

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// The timeline's child_procs is whatever Config.Children reports, sampled once per line.
func TestChildrenFillsChildProcs(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{})

	var children atomic.Int64
	children.Store(3)

	b, err := Start(Config{
		Dir:      dir,
		Interval: time.Second,
		Clock:    clk,
		Children: func() int { return int(children.Load()) },
		sampled:  sampled,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Baseline is already written with 3. Move the count and step the clock, so the
	// timeline shows the counter tracking the process rather than a value cached at start.
	children.Store(7)
	clk.Advance(time.Second)
	<-sampled

	if err := b.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	samples := readTimeline(t, dir)
	if len(samples) < 2 {
		t.Fatalf("got %d samples, want at least 2", len(samples))
	}
	if got := samples[0].ChildProcs; got != 3 {
		t.Errorf("baseline child_procs is %d, want 3", got)
	}
	if got := samples[1].ChildProcs; got != 7 {
		t.Errorf("second sample's child_procs is %d, want 7", got)
	}
}

// A Config with no Children records Unknown, never zero. Zero is a claim that the process
// holds no children; an application that never said what it spawned has made no such
// claim, and the watchdog must treat the gap as a gap. Reporting zero here would fit a
// flat line through a leak the process is not reporting.
func TestNoChildrenRecordsUnknownNotZero(t *testing.T) {
	dir := t.TempDir()

	b, err := Start(Config{Dir: dir, Interval: time.Second, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	samples := readTimeline(t, dir)
	if len(samples) == 0 {
		t.Fatal("no samples written")
	}
	for i, s := range samples {
		if s.ChildProcs != Unknown {
			t.Errorf("sample %d reports child_procs %d with no Children configured, want Unknown (%d)",
				i, s.ChildProcs, Unknown)
		}
	}
}

// A sample whose child_procs is Unknown is a gap for the detector, not a data point: the
// watchdog restarts the counter's window rather than fitting a slope across it. This is
// the behaviour that makes "no registry" honest instead of quietly flat.
func TestUnknownChildProcsIsAGapForTheDetector(t *testing.T) {
	if _, ok := counterValue(Sample{ChildProcs: Unknown}, CounterChildProcs); ok {
		t.Error("Unknown child_procs was read as a value; it must be a gap")
	}
	v, ok := counterValue(Sample{ChildProcs: 4}, CounterChildProcs)
	if !ok || v != 4 {
		t.Errorf("child_procs 4 read as (%v, %v), want (4, true)", v, ok)
	}
}
