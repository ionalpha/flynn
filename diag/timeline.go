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
	// HeapLiveBytes is the heap the last garbage collection found reachable, or
	// Unknown where the runtime does not report it. HeapAllocBytes read mid-cycle
	// counts garbage that has not been collected yet, so it rises and falls with the
	// collector; this one moves only when retention moves, which is why the leak
	// watchdog fits its slope and not HeapAllocBytes'.
	HeapLiveBytes int64 `json:"heap_live_bytes"`
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
	// Extra carries the application-supplied counters from Config.Counters, keyed by
	// counter name. It is absent from a sample taken with no such counters, and a
	// counter that could not be measured this sample carries Unknown.
	Extra map[string]float64 `json:"extra,omitempty"`
}

// timelineWriter samples the process on a clock-driven interval and appends one
// JSON object per line to the timeline member.
type timelineWriter struct {
	clk      clock.Timing
	interval time.Duration
	counters []Counter

	// observe, when non-nil, receives every sample after it is written. The leak
	// watchdog is the only observer: it rides this sampler rather than starting a
	// second one, so diag owns exactly one long-lived goroutine whether or not the
	// watchdog is on, and the watchdog's window is literally the timeline's.
	observe func(Sample)

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
func (b *Bundle) startTimeline(observer func(Sample)) (*timelineWriter, error) {
	f, err := b.create(MemberTimeline)
	if err != nil {
		return nil, err
	}

	w := &timelineWriter{
		clk:      b.clk,
		interval: b.cfg.Interval,
		counters: b.cfg.Counters,
		observe:  observer,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		sampled:  b.cfg.sampled,
		f:        f,
		enc:      json.NewEncoder(f),
	}
	w.write(true)

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
			w.write(true)
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
//
// The final sample is not offered to the watchdog. A leak reported as the process
// exits tells an operator nothing the bundle's own exit-time profiles do not
// already say, and dumping one would race the manifest that is about to hash the
// directory.
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

// write appends one sample and, when asked, offers it to the observer. The first
// write error is kept and reported by stop; later writes still run, because a
// timeline that lost a line in the middle is worth more than one abandoned at it.
//
// The observer runs after the line is on disk and outside the lock, on this
// goroutine. It may block: the watchdog writes a heap profile when it fires, and
// the next sample waits for it rather than describing the profiler's own work.
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
		ChildProcs:     childProcs(),
		Extra:          extra,
	}
}

// The runtime metrics behind HeapLiveBytes. The first is the heap the last
// collection found reachable: unlike MemStats.HeapAlloc it does not count garbage
// awaiting collection, and reading it does not stop the world. The second is how
// many collections have completed, which is what says whether the first means
// anything yet.
const (
	heapLiveMetric = "/gc/heap/live:bytes"
	gcCyclesMetric = "/gc/cycles/total:gc-cycles"
)

// heapLiveBytes reads the live heap, or Unknown when the runtime cannot answer.
//
// A metric this runtime does not publish arrives as KindBad. Reported as a zero it
// would be indistinguishable from a process that has collected nothing, and the
// counter would be silently unwatched for the life of the binary, so it is Unknown.
// TestHeapLiveBytesIsReadable is what notices if a future toolchain renames either.
func heapLiveBytes() int64 {
	s := []metrics.Sample{{Name: heapLiveMetric}, {Name: gcCyclesMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 || s[1].Value.Kind() != metrics.KindUint64 {
		return Unknown
	}
	return liveHeap(s[0].Value.Uint64(), s[1].Value.Uint64())
}

// liveHeap turns the two metrics into a counter the detector can fit.
//
// Before the first collection the runtime reports a live heap of zero, because it
// has not yet found out what is reachable. That zero is not a measurement, and a
// detector handed it would see a process's whole startup heap appear between two
// samples: zero, zero, and then every byte the process was already holding. Fitted
// across a window, a real leak and an ordinary program that simply had not collected
// yet look identical. So an uncollected process reports Unknown, the window
// restarts, and the first slope is fitted only across values that mean something.
func liveHeap(live, cycles uint64) int64 {
	if cycles == 0 {
		return Unknown
	}
	if live > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(live)
}

// writeSamples encodes samples as JSONL, the same shape the timeline member
// carries, so the window a leak fired on is read by whatever reads runtime.jsonl.
func writeSamples(w io.Writer, samples []Sample) error {
	enc := json.NewEncoder(w)
	for _, s := range samples {
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	return nil
}
