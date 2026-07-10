package diag

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Sample is one line of the timeline: the process's runtime shape at a moment.
// A single sample says little; the series is what matters, because a leak is a
// slope and not a value. D3's watchdog fits that slope live, and `flynn diagnose`
// diffs two bundles' series after the fact.
//
// Counters the platform cannot report arrive as -1 rather than 0, so a reader
// never mistakes "not measurable here" for "none".
type Sample struct {
	// T is when the sample was taken, from the injected clock.
	T time.Time `json:"t"`
	// Goroutines is the live goroutine count. A monotone rise is the clearest leak
	// signal the runtime offers.
	Goroutines int `json:"goroutines"`
	// Threads is the number of OS threads the runtime has ever created. It only
	// grows, so a jump means blocking syscalls or cgo, not garbage.
	Threads int `json:"threads"`
	// HeapAllocBytes is live heap memory; HeapObjects is the count of live objects.
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapObjects    uint64 `json:"heap_objects"`
	// HeapSysBytes is heap memory obtained from the OS: the ceiling the process has
	// actually reached, which is what an operator's memory limit sees.
	HeapSysBytes uint64 `json:"heap_sys_bytes"`
	// Mallocs and Frees are cumulative object counts. Their difference is HeapObjects,
	// and their rate is allocation pressure the CPU profile alone will not show.
	Mallocs uint64 `json:"mallocs"`
	Frees   uint64 `json:"frees"`
	// NumGC and GCPauseTotalNs are cumulative garbage-collector work.
	NumGC          uint32 `json:"num_gc"`
	GCPauseTotalNs uint64 `json:"gc_pause_total_ns"`
	// OpenFDs is the process's open file descriptor (or handle) count, or -1 where
	// the platform does not expose it. A rise here outlives any Go-level leak: it
	// ends in "too many open files".
	OpenFDs int `json:"open_fds"`
	// ChildProcs is how many live processes name this one as parent, or -1 where the
	// platform does not expose it. The agent spawns sandboxed commands; one that is
	// never reaped shows up here and nowhere else.
	ChildProcs int `json:"child_procs"`
}

// timelineWriter samples the process on a clock-driven interval and appends one
// JSON object per line to the timeline member.
type timelineWriter struct {
	clk      clock.Timing
	interval time.Duration

	quit chan struct{}
	done chan struct{}

	// sampled, when non-nil, receives after every sample is written. Only a test
	// sets it, to step a Manual clock without racing the sampler's re-arm.
	sampled chan struct{}

	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
	err error
}

// startTimeline opens the timeline member, writes the baseline sample, and starts
// the sampler. The baseline matters: a growth slope needs a first point, and the
// process's shape before any work happened is the only honest one to fit against.
func (b *Bundle) startTimeline() (*timelineWriter, error) {
	f, err := b.create(MemberTimeline)
	if err != nil {
		return nil, err
	}

	w := &timelineWriter{
		clk:      b.clk,
		interval: b.cfg.Interval,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		sampled:  b.cfg.sampled,
		f:        f,
		enc:      json.NewEncoder(f),
	}
	w.write()

	// The timer is armed here rather than inside loop so that it exists before Start
	// returns. A Manual clock only fires timers that are already registered when it
	// advances, so arming in the goroutine would race a test that advances immediately.
	go w.loop(w.clk.NewTimer(w.interval))
	return w, nil
}

// loop samples on every tick of the injected clock until stop closes quit. A
// single-shot timer is re-armed rather than a ticker used, because clock.Timing
// hands out timers a Manual clock can drive; a real ticker would leak wall time
// into a deterministic test.
func (w *timelineWriter) loop(t clock.Timer) {
	defer close(w.done)
	defer t.Stop()

	for {
		select {
		case <-w.quit:
			return
		case <-t.C():
			w.write()
			t.Reset(w.interval)
			if w.sampled != nil {
				// A test steps the clock one tick at a time and waits here. Selecting on
				// quit as well keeps stop from deadlocking against a test that gave up.
				select {
				case w.sampled <- struct{}{}:
				case <-w.quit:
					return
				}
			}
		}
	}
}

// stop halts the sampler, writes the final sample, and closes the member. The
// final sample is what the exit-time profiles are read against.
func (w *timelineWriter) stop() error {
	close(w.quit)
	<-w.done

	w.write()

	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.err
	if cerr := w.f.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("diag: close %s: %w", MemberTimeline, cerr)
	}
	return err
}

// write appends one sample. The first write error is kept and reported by stop;
// later writes still run, because a timeline that lost a line in the middle is
// worth more than one abandoned at it.
func (w *timelineWriter) write() {
	s := w.sample()

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(s); err != nil && w.err == nil {
		w.err = fmt.Errorf("diag: write %s: %w", MemberTimeline, err)
	}
}

// sample reads the process's current runtime shape.
//
// ReadMemStats briefly stops the world. That is the reason this samples on an
// interval rather than continuously, and the reason the whole package is off
// unless a bundle was asked for.
func (w *timelineWriter) sample() Sample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	threads := 0
	if p := pprof.Lookup("threadcreate"); p != nil {
		threads = p.Count()
	}

	return Sample{
		T:              w.clk.Now(),
		Goroutines:     runtime.NumGoroutine(),
		Threads:        threads,
		HeapAllocBytes: ms.HeapAlloc,
		HeapObjects:    ms.HeapObjects,
		HeapSysBytes:   ms.HeapSys,
		Mallocs:        ms.Mallocs,
		Frees:          ms.Frees,
		NumGC:          ms.NumGC,
		GCPauseTotalNs: ms.PauseTotalNs,
		OpenFDs:        openFDs(),
		ChildProcs:     childProcs(),
	}
}
