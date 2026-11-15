package diag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/observe"
)

// EnvLeakWatch enables the watchdog without a command-line change, equivalent to
// --leak-watch; any strconv.ParseBool true value enables it. It needs a bundle
// directory, because the watchdog samples the timeline and dumps into the bundle.
const EnvLeakWatch = "FLYNN_LEAK_WATCH"

// The counters the watchdog reads off a Sample. Each names a distinct leak class: a
// run that never closes a file or never reaps a command shows a flat heap and a flat
// goroutine count until it fails.
const (
	CounterGoroutines = "goroutines"
	CounterHeapLive   = "heap_live_bytes"
	CounterOpenFDs    = "open_fds"
	CounterChildProcs = "child_procs"
)

// DefaultWindow is how many consecutive samples the detector fits before it can
// fire: twelve seconds at DefaultInterval, growth no goal loop sustains and every
// leak does.
const DefaultWindow = 12

// Threshold is what a counter must do across a full window to be called a leak.
// It must clear both floors: MinSlope rejects growth too slow to count over a
// process's life, MinDelta growth too small to count at all.
type Threshold struct {
	// MinSlope is the least least-squares slope, in counter units per sample.
	MinSlope float64
	// MinDelta is the least rise from the window's first sample to its last.
	MinDelta float64
}

// Counter is an application gauge sampled beside the built-in ones and recorded in
// the timeline under its name, for a quantity this package cannot see whose steady
// state is flat.
//
// Read runs on the sampler goroutine once per interval and must not block, because
// it delays the timeline. It returns Unknown for a value it cannot measure, which
// the detector treats as a gap.
type Counter struct {
	Name string
	Read func() float64
}

// safeCounterName reports whether name is a single plain filename component. A firing
// counter's name becomes part of leak.<name>.<seq>.<member>: a separator writes into a
// missing directory and "." or ".." leaves the bundle. Start refuses an unsafe name.
func safeCounterName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// DefaultThresholds are the floors for the built-in counters: a multi-turn run with
// fan-out does not fire and a leak does.
//
// The goroutine and fd floors sit above a fan-out's transient population, which
// returns to baseline when the child is reaped. The heap floor is 64 MiB that never
// reverses across twelve collections, which retention produces and a working set
// does not. The child-process floor is low because nothing legitimately holds eight
// unreaped children.
func DefaultThresholds() map[string]Threshold {
	return map[string]Threshold{
		CounterGoroutines: {MinSlope: 1, MinDelta: 64},
		CounterHeapLive:   {MinSlope: 1 << 20, MinDelta: 64 << 20},
		CounterOpenFDs:    {MinSlope: 0.5, MinDelta: 32},
		CounterChildProcs: {MinSlope: 0.25, MinDelta: 8},
	}
}

// LeakConfig turns the watchdog on. A nil *LeakConfig on Config disables it and
// leaves the sampler unchanged.
type LeakConfig struct {
	// Window is how many samples the detector fits; zero means DefaultWindow. Below 4
	// is rejected, because three points admit any slope.
	Window int

	// Repeat lets a counter fire more than once. By default it fires once, so an
	// unattended process does not fill a disk with dumps of a leak already recorded.
	Repeat bool

	// Thresholds maps counter name to its floors; nil means DefaultThresholds. A
	// non-nil map is used as given: a counter with no threshold is recorded and never
	// fires.
	Thresholds map[string]Threshold

	// Logger receives a Warn record on every firing; nil means observe.NopLogger, which
	// leaves the dump as the only report.
	Logger observe.Logger
}

// Finding is one firing: a counter that grew, by how much, over which samples.
type Finding struct {
	Counter string
	// Slope is the fitted growth, in counter units per sample.
	Slope float64
	// Delta is the rise from the window's first sample to its last.
	Delta float64
	// First and Last are the window's bounding values.
	First, Last float64
	// Window is the fitted samples, oldest first.
	Window []Sample
	// Dumps names the files written, relative to the bundle directory; empty when the
	// dump failed, which the log record reports.
	Dumps []string
}

// At returns when the growth was detected: the last sample's time.
func (f Finding) At() time.Time {
	if len(f.Window) == 0 {
		return time.Time{}
	}
	return f.Window[len(f.Window)-1].T
}

// watchdog fits every watched counter over a rolling window and dumps evidence when
// one grows. The timeline sampler drives it, so diag owns one long-lived goroutine.
type watchdog struct {
	cfg    LeakConfig
	log    observe.Logger
	window int

	// series holds the last window values per watched counter, oldest first. The
	// samples are shared, so a dump carries whole lines.
	series  map[string][]float64
	samples []Sample

	fired map[string]int
	dump  func(Finding) ([]string, error)
}

// newWatchdog validates cfg, so a bad one fails at Start rather than at the first
// sample.
func newWatchdog(cfg LeakConfig, dump func(Finding) ([]string, error)) (*watchdog, error) {
	w := &watchdog{
		cfg:    cfg,
		log:    cfg.Logger,
		window: cfg.Window,
		series: make(map[string][]float64),
		fired:  make(map[string]int),
		dump:   dump,
	}
	if w.log == nil {
		w.log = observe.NopLogger{}
	}
	if w.window == 0 {
		w.window = DefaultWindow
	}
	if w.window < 4 {
		return nil, fmt.Errorf("diag: leak watch window %d is too short to fit a slope (minimum 4)", w.window)
	}
	if w.cfg.Thresholds == nil {
		w.cfg.Thresholds = DefaultThresholds()
	}
	for name, t := range w.cfg.Thresholds {
		if t.MinSlope <= 0 || t.MinDelta <= 0 {
			return nil, fmt.Errorf("diag: leak watch threshold for %q has a non-positive floor (slope %g, delta %g); a zero floor fires on noise", name, t.MinSlope, t.MinDelta)
		}
	}
	return w, nil
}

// push feeds one sample to every watched counter and dumps evidence for each that
// fired. It runs on the sampler goroutine, so a dump delays the next sample, which
// would otherwise describe the profiler writing the heap.
func (w *watchdog) push(s Sample) {
	w.samples = appendCapped(w.samples, s, w.window)

	for name, threshold := range w.cfg.Thresholds {
		v, ok := counterValue(s, name)
		if !ok {
			// A gap (an unreportable or missing counter) restarts the window, because a
			// slope fitted across a gap is fiction.
			delete(w.series, name)
			continue
		}
		w.series[name] = appendCapped(w.series[name], v, w.window)

		// Fit only a full window, so a counter cannot fire on the first four samples of
		// the hundred an operator asked for.
		if len(w.series[name]) < w.window {
			continue
		}
		if !w.cfg.Repeat && w.fired[name] > 0 {
			continue
		}
		f, ok := detect(w.series[name], threshold)
		if !ok {
			continue
		}

		// Restart the window after a firing, so a Repeat watchdog does not fire on every
		// later sample of the same leak.
		w.series[name] = nil
		w.fired[name]++

		f.Counter = name
		f.Window = append([]Sample(nil), w.samples...)
		w.report(f)
	}
}

// report writes the dump and logs the finding. A failed dump is still logged,
// because the counter and slope come first and a full disk is the likeliest cause.
func (w *watchdog) report(f Finding) {
	dumps, err := w.dump(f)
	f.Dumps = dumps

	fields := []observe.Field{
		observe.String("counter", f.Counter),
		observe.Float64("slope_per_sample", f.Slope),
		observe.Float64("delta", f.Delta),
		observe.Float64("first", f.First),
		observe.Float64("last", f.Last),
		observe.Int("window", len(f.Window)),
		observe.Any("dumps", f.Dumps),
	}
	if err != nil {
		fields = append(fields, observe.Err(err))
	}
	w.log.Warn(context.Background(), "diag: sustained growth, possible leak", fields...)
}

// detect reports whether a full window shows sustained growth clearing both floors.
// push fills in the name. Each condition rejects a shape the others admit:
//
//   - Staircase: three blocks, each entirely above the one before. It rejects a
//     sawtooth, a spike, a fan-out that returns to baseline and a single step, yet
//     survives a live heap's rise and fall between collections.
//   - Slope: the least-squares fit is at least MinSlope.
//   - Delta: the last sample is at least MinDelta above the first.
//
// Growth that pauses inside the window waits for the next one, because a deferred
// report costs less than the false positive that gets --leak-watch turned off.
func detect(vs []float64, t Threshold) (Finding, bool) {
	if len(vs) < 4 {
		return Finding{}, false
	}

	first, last := vs[0], vs[len(vs)-1]
	delta := last - first
	if delta < t.MinDelta {
		return Finding{}, false
	}
	if !staircase(vs) {
		return Finding{}, false
	}
	slope := leastSquaresSlope(vs)
	if slope < t.MinSlope {
		return Finding{}, false
	}

	return Finding{Slope: slope, Delta: delta, First: first, Last: last}, true
}

// staircase reports whether the window rises in three separated blocks, each sample
// of a block above every sample of the one before. Two halves cannot tell a ramp
// from a step, and a step is a fan-out that raised the floor. The outer blocks take
// n/3 samples each, floored, and the middle the rest.
func staircase(vs []float64) bool {
	third := len(vs) / 3
	if third == 0 {
		return false
	}
	first, middle, last := vs[:third], vs[third:len(vs)-third], vs[len(vs)-third:]
	return above(middle, first) && above(last, middle)
}

func above(hi, lo []float64) bool {
	loMax := math.Inf(-1)
	for _, v := range lo {
		loMax = math.Max(loMax, v)
	}
	hiMin := math.Inf(1)
	for _, v := range hi {
		hiMin = math.Min(hiMin, v)
	}
	return hiMin > loMax
}

// leastSquaresSlope fits vs against indices 0..n-1, in counter units per sample. The
// denominator depends only on n, so the fit is one pass.
func leastSquaresSlope(vs []float64) float64 {
	n := float64(len(vs))
	meanX := (n - 1) / 2

	var meanY float64
	for _, v := range vs {
		meanY += v
	}
	meanY /= n

	var num, den float64
	for i, v := range vs {
		dx := float64(i) - meanX
		num += dx * (v - meanY)
		den += dx * dx
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// counterValue reads a named counter off a sample. It reports false for Unknown and
// for a name no sample carries, so the detector sees a gap where a zero would invent
// a slope.
func counterValue(s Sample, name string) (float64, bool) {
	switch name {
	case CounterGoroutines:
		return float64(s.Goroutines), true
	case CounterHeapLive:
		if s.HeapLiveBytes == Unknown {
			return 0, false
		}
		return float64(s.HeapLiveBytes), true
	case CounterOpenFDs:
		if s.OpenFDs == Unknown {
			return 0, false
		}
		return float64(s.OpenFDs), true
	case CounterChildProcs:
		if s.ChildProcs == Unknown {
			return 0, false
		}
		return float64(s.ChildProcs), true
	}
	v, ok := s.Extra[name]
	if !ok || v == Unknown {
		return 0, false
	}
	return v, true
}

// appendCapped appends v and keeps the newest n elements in the same backing array,
// because a window that reallocates every sample is a leak in the leak detector. The
// first append allocates n, so append's doubling never exceeds it.
func appendCapped[T any](xs []T, v T, n int) []T {
	if xs == nil {
		xs = make([]T, 0, n)
	}
	if len(xs) < n {
		return append(xs, v)
	}
	copy(xs, xs[1:])
	xs[len(xs)-1] = v
	return xs
}

// dumpLeak writes one finding's evidence into the bundle: labelled goroutine
// profile, stacks, heap profile and the firing window, returning the member names.
// It forces no GC, because the heap profile already describes the last collection
// and a forced one would change the process being observed.
func (b *Bundle) dumpLeak(f Finding) ([]string, error) {
	prefix := fmt.Sprintf("leak.%s.%d.", f.Counter, b.leakSeq(f.Counter))

	var (
		names []string
		errs  []error
	)
	write := func(name string, fn func(io.Writer) error) {
		if err := b.writeAtomic(name, fn); err != nil {
			errs = append(errs, err)
			return
		}
		names = append(names, name)
	}

	// debug=1 folds stacks into counts with their pprof labels, which name the
	// action that left the goroutines parked.
	write(prefix+"goroutine.labels.txt", func(w io.Writer) error { return pprof.Lookup("goroutine").WriteTo(w, 1) })
	write(prefix+"goroutine.txt", func(w io.Writer) error { return pprof.Lookup("goroutine").WriteTo(w, 2) })
	write(prefix+MemberHeap, func(w io.Writer) error { return pprof.Lookup("heap").WriteTo(w, 0) })
	write(prefix+"window.jsonl", func(w io.Writer) error { return writeSamples(w, f.Window) })

	return names, errors.Join(errs...)
}

// leakSeq numbers a counter's dumps, so a Repeat watchdog keeps the first firing.
func (b *Bundle) leakSeq(counter string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.leakDumps[counter]
	b.leakDumps[counter] = n + 1
	return n
}

// writeAtomic writes a bundle member through fsatomic, so a reader watching the
// directory never opens a half-written profile and a bundle survives a crash. It
// streams, because a leaking process is the worst one to hold a whole dump in memory.
func (b *Bundle) writeAtomic(name string, fn func(io.Writer) error) error {
	if err := fsatomic.WriteStream(filepath.Join(b.cfg.Dir, name), 0o600, fn); err != nil {
		return fmt.Errorf("diag: write %s: %w", name, err)
	}
	return nil
}
