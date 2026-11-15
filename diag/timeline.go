package diag

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Sample is one timeline line: the process's runtime state at a moment. The leak
// watchdog fits the series live and `flynn diagnose` diffs two bundles' series.
// Unreportable counters are -1, so "not measurable" never reads as "none".
type Sample struct {
	// T is when the sample was taken, from the injected clock.
	T time.Time `json:"t"`
	// Goroutines is the live goroutine count; a monotone rise is the clearest leak.
	Goroutines int `json:"goroutines"`
	// Threads is the OS threads the runtime ever created. It only grows, so a jump
	// means blocking syscalls or cgo.
	Threads int `json:"threads"`
	// HeapAllocBytes is live heap memory; HeapObjects is the count of live objects.
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapObjects    uint64 `json:"heap_objects"`
	// HeapLiveBytes is the heap the last collection found reachable, or Unknown. It
	// moves only with retention, unlike HeapAllocBytes, which rises and falls with
	// the collector, so the watchdog fits this one.
	HeapLiveBytes int64 `json:"heap_live_bytes"`
	// HeapSysBytes is heap memory obtained from the OS, what a memory limit sees.
	HeapSysBytes uint64 `json:"heap_sys_bytes"`
	// Mallocs and Frees are cumulative; their rate is allocation pressure the CPU
	// profile does not show.
	Mallocs uint64 `json:"mallocs"`
	Frees   uint64 `json:"frees"`
	// NumGC and GCPauseTotalNs are cumulative garbage-collector work.
	NumGC          uint32 `json:"num_gc"`
	GCPauseTotalNs uint64 `json:"gc_pause_total_ns"`
	// OpenFDs is the open descriptor or handle count, or -1. A rise ends in "too
	// many open files".
	OpenFDs int `json:"open_fds"`
	// ChildProcs is the started and unreaped children from Config.Children, or -1
	// with none supplied.
	ChildProcs int `json:"child_procs"`
	// Extra holds Config.Counters by name; absent without counters, Unknown for one
	// unmeasured this sample.
	Extra map[string]float64 `json:"extra,omitempty"`
}

// timelineWriter samples on a clock-driven interval and appends JSONL.
type timelineWriter struct {
	clk      clock.Timing
	interval time.Duration
	counters []Counter
	children func() int

	// observe, when non-nil, receives every written sample. Only the watchdog sets it,
	// so its window is the timeline's and diag runs one sampler.
	observe func(Sample)

	quit chan struct{}
	done chan struct{}

	// sampled is set only by tests, to step a Manual clock without racing the re-arm.
	sampled chan struct{}

	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
	err error
}

// startTimeline opens the timeline, writes the baseline sample and starts the
// sampler. A growth slope needs the pre-work baseline as its first point.
func (b *Bundle) startTimeline(observer func(Sample)) (*timelineWriter, error) {
	f, err := b.create(MemberTimeline)
	if err != nil {
		return nil, err
	}

	w := &timelineWriter{
		clk:      b.clk,
		interval: b.cfg.Interval,
		counters: b.cfg.Counters,
		children: b.cfg.Children,
		observe:  observer,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		sampled:  b.cfg.sampled,
		f:        f,
		enc:      json.NewEncoder(f),
	}
	w.write(true)

	// Arm the timer before Start returns: a Manual clock fires only registered
	// timers, so arming in the goroutine races a test that advances at once.
	go w.loop(w.clk.NewTimer(w.interval))
	return w, nil
}

// loop samples on each tick until stop closes quit. It re-arms a clock.Timing
// timer, because a real ticker would leak wall time into a deterministic test.
func (w *timelineWriter) loop(t clock.Timer) {
	defer close(w.done)
	defer t.Stop()

	for {
		select {
		case <-w.quit:
			return
		case <-t.C():
			w.write(true)
			t.Reset(w.interval)
			if w.sampled != nil {
				// Selecting on quit keeps stop from deadlocking against a test that gave up.
				select {
				case w.sampled <- struct{}{}:
				case <-w.quit:
					return
				}
			}
		}
	}
}

// stop halts the sampler, writes the final sample the exit profiles are read
// against, and closes the member. The final sample skips the watchdog, because a
// dump at exit adds nothing and would race the manifest's hashing.
func (w *timelineWriter) stop() error {
	close(w.quit)
	<-w.done

	w.write(false)

	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.err
	if cerr := w.f.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("diag: close %s: %w", MemberTimeline, cerr)
	}
	return err
}

// write appends one sample and, when observed, offers it to the observer after the
// line is on disk, outside the lock. The first write error is kept for stop and
// later writes still run, because a timeline missing one line beats one cut short.
// The observer may block while the watchdog writes a heap profile.
func (w *timelineWriter) write(observed bool) {
	s := w.sample()

	w.mu.Lock()
	if err := w.enc.Encode(s); err != nil && w.err == nil {
		w.err = fmt.Errorf("diag: write %s: %w", MemberTimeline, err)
	}
	w.mu.Unlock()

	if observed && w.observe != nil {
		w.observe(s)
	}
}

// childProcs reads Config.Children, or Unknown with none: a missing Children is
// not evidence of no children.
func (w *timelineWriter) childProcs() int {
	if w.children == nil {
		return Unknown
	}
	return w.children()
}

// sample reads the process's runtime state. ReadMemStats briefly stops the world,
// which is why sampling is periodic and the package is off without a bundle.
func (w *timelineWriter) sample() Sample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	threads := 0
	if p := pprof.Lookup("threadcreate"); p != nil {
		threads = p.Count()
	}

	var extra map[string]float64
	if len(w.counters) > 0 {
		extra = make(map[string]float64, len(w.counters))
		for _, c := range w.counters {
			extra[c.Name] = c.Read()
		}
	}

	return Sample{
		T:              w.clk.Now(),
		Goroutines:     runtime.NumGoroutine(),
		Threads:        threads,
		HeapAllocBytes: ms.HeapAlloc,
		HeapObjects:    ms.HeapObjects,
		HeapLiveBytes:  heapLiveBytes(),
		HeapSysBytes:   ms.HeapSys,
		Mallocs:        ms.Mallocs,
		Frees:          ms.Frees,
		NumGC:          ms.NumGC,
		GCPauseTotalNs: ms.PauseTotalNs,
		OpenFDs:        openFDs(),
		ChildProcs:     w.childProcs(),
		Extra:          extra,
	}
}

// Runtime metrics behind HeapLiveBytes: the heap the last collection found
// reachable, read without stopping the world, and the completed collection count,
// which says whether the first means anything yet.
const (
	heapLiveMetric = "/gc/heap/live:bytes"
	gcCyclesMetric = "/gc/cycles/total:gc-cycles"
)

// heapLiveBytes reads the live heap, or Unknown when the runtime does not publish
// the metric (KindBad); a zero would leave the counter unwatched for the binary's
// life. TestHeapLiveBytesIsReadable catches a toolchain renaming either.
func heapLiveBytes() int64 {
	s := []metrics.Sample{{Name: heapLiveMetric}, {Name: gcCyclesMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 || s[1].Value.Kind() != metrics.KindUint64 {
		return Unknown
	}
	return liveHeap(s[0].Value.Uint64(), s[1].Value.Uint64())
}

// liveHeap turns the two metrics into a fittable counter. Before the first
// collection the runtime reports zero live heap, which would make the startup heap
// appear between two samples and look like a leak, so it reports Unknown and the
// window restarts.
func liveHeap(live, cycles uint64) int64 {
	if cycles == 0 {
		return Unknown
	}
	if live > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(live)
}

// writeSamples encodes samples as JSONL, the timeline's shape, so whatever reads
// runtime.jsonl reads a leak window.
func writeSamples(w io.Writer, samples []Sample) error {
	enc := json.NewEncoder(w)
	for _, s := range samples {
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	return nil
}
