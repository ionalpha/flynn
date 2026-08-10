// The leak watchdog. Where the rest of the package captures a bundle for a human
// to read afterwards, this watches the same counters live and writes evidence at
// the moment growth starts, unattended, on a 24/7 box or in CI.
//
// A leak is a slope, not a value. No single sample can tell a leak from a goal
// loop that legitimately allocated: the detector therefore fits a window and
// fires only on growth that is sustained. The false-positive rate is the whole
// design problem, because a watchdog that cries wolf gets turned off, and a
// watchdog that is off protects nothing.

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

// EnvLeakWatch enables the watchdog on a process whose command line cannot be
// changed, equivalent to --leak-watch. Any value Go's strconv.ParseBool accepts as
// true enables it. It has no effect without a bundle directory, because the
// watchdog samples the bundle's timeline and dumps into the bundle.
const EnvLeakWatch = "FLYNN_LEAK_WATCH"

// The counters the watchdog knows how to read off a Sample. Each names a distinct
// leak class, because one profile sees only one of them: a run that never closes a
// file, or never reaps a sandboxed command, shows a flat heap and a flat goroutine
// count right up to the moment it fails.
const (
	CounterGoroutines = "goroutines"
	CounterHeapLive   = "heap_live_bytes"
	CounterOpenFDs    = "open_fds"
	CounterChildProcs = "child_procs"
)

// DefaultWindow is how many consecutive samples the detector fits before it will
// fire. At DefaultInterval that is twelve seconds of growth that never once
// reverses, which no goal loop the agent runs produces and every real leak does.
const DefaultWindow = 12

// Threshold is what a counter must do, across a full window, to be called a leak.
// Both floors must be cleared, and they answer different objections: MinSlope
// rejects growth too slow to matter over the life of a process, and MinDelta
// rejects growth too small to matter at all.
type Threshold struct {
	// MinSlope is the least least-squares slope, in counter units per sample, that
	// counts as growth.
	MinSlope float64
	// MinDelta is the least rise from the window's first sample to its last, in
	// counter units, that counts as growth.
	MinDelta float64
}

// Counter is an application-supplied gauge sampled alongside the built-in ones and
// recorded in the timeline under its name. It is how counters this package cannot
// know about are watched: the number of temporary directories created and not
// removed, the size of the event log on disk, any other quantity whose steady state
// is flat.
//
// Read runs on the sampler goroutine, once per interval, and must not block: it
// delays the timeline while it runs. It returns Unknown for a value it cannot
// measure this sample, which the detector treats as a gap rather than as a zero.
type Counter struct {
	Name string
	Read func() float64
}

// safeCounterName reports whether name may be spliced into a leak dump filename. A
// firing counter's name becomes part of the path leak.<name>.<seq>.<member>, so it has
// to be a single plain filename component: a name carrying a path separator writes into
// a directory that was never created, and "." or ".." walks out of the bundle. Start
// checks this so an unsafe name is refused before any evidence is written.
func safeCounterName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// DefaultThresholds are the floors for the built-in counters, chosen so that a
// multi-turn run with fan-out does not fire and a real leak does.
//
// The goroutine and fd floors sit above the transient population of a fan-out (a
// child agent, its bus subscribers, and the descriptors they hold, all of which
// return to baseline when the child is reaped). The heap floor is a rise of 64 MiB
// that never once reverses across twelve garbage collections, which retained
// garbage produces and a working set does not. The child-process floor is low
// because nothing in the agent legitimately holds eight unreaped children.
func DefaultThresholds() map[string]Threshold {
	return map[string]Threshold{
		CounterGoroutines: {MinSlope: 1, MinDelta: 64},
		CounterHeapLive:   {MinSlope: 1 << 20, MinDelta: 64 << 20},
		CounterOpenFDs:    {MinSlope: 0.5, MinDelta: 32},
		CounterChildProcs: {MinSlope: 0.25, MinDelta: 8},
	}
}

// LeakConfig turns the watchdog on. A nil *LeakConfig on Config is the disabled
// watchdog: nothing is fitted, nothing is dumped, and the sampler behaves exactly
// as it does without it.
type LeakConfig struct {
	// Window is how many samples the detector fits. Zero means DefaultWindow. A
	// window shorter than 4 is rejected: three points admit any slope.
	Window int

	// Repeat lets a counter fire more than once in a process. By default a counter
	// fires once, because the second dump of a leak that is still leaking says what
	// the first already said, and an unattended process must not fill a disk with
	// evidence of a fact already recorded.
	Repeat bool

	// Thresholds maps counter name to the floors that counter must clear. Nil means
	// DefaultThresholds. A non-nil map is used as given, so a caller that names only
	// CounterGoroutines watches only goroutines: a counter with no threshold is
	// recorded in the timeline and never fires.
	Thresholds map[string]Threshold

	// Logger receives a Warn record on every firing. Nil means observe.NopLogger, in
	// which case the dump on disk is the only report.
	Logger observe.Logger
}

// Finding is one firing: a counter that grew, by how much, over which samples.
type Finding struct {
	// Counter is the counter that fired.
	Counter string
	// Slope is the fitted growth, in counter units per sample.
	Slope float64
	// Delta is the rise from the window's first sample to its last.
	Delta float64
	// First and Last are the window's bounding values.
	First, Last float64
	// Window is the samples the detector fitted, oldest first.
	Window []Sample
	// Dumps names the files written for this finding, relative to the bundle
	// directory. It is empty when the dump failed, which the log record reports.
	Dumps []string
}

// At returns when the growth was detected, which is the last sample's time.
func (f Finding) At() time.Time {
	if len(f.Window) == 0 {
		return time.Time{}
	}
	return f.Window[len(f.Window)-1].T
}

// watchdog fits every watched counter over a rolling window and dumps evidence
// when one of them grows. It is driven by the timeline sampler and owns no
// goroutine of its own: the one long-lived goroutine diag owns is the sampler,
// which is the first thing a leak gate checks.
type watchdog struct {
	cfg    LeakConfig
	log    observe.Logger
	window int

	// series holds the last window samples per watched counter, oldest first, and
	// the samples they were read from. The samples are shared across counters, so a
	// dump carries the whole line and not just the counter that fired.
	series  map[string][]float64
	samples []Sample

	fired map[string]int
	dump  func(Finding) ([]string, error)
}

// newWatchdog validates cfg and returns a watchdog, or an error a caller sees at
// Start rather than at the first sample.
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
// fired. It runs on the sampler goroutine, so a dump delays the next sample; that
// is deliberate. A dump is rare, and a sample taken while the heap is being written
// out describes the profiler rather than the process.
func (w *watchdog) push(s Sample) {
	w.samples = appendCapped(w.samples, s, w.window)

	for name, threshold := range w.cfg.Thresholds {
		v, ok := counterValue(s, name)
		if !ok {
			// The platform cannot report this counter, or an Extra counter went missing
			// from this sample. Either way the window has a gap, and a slope fitted
			// across a gap is fiction: start over rather than fit it.
			delete(w.series, name)
			continue
		}
		w.series[name] = appendCapped(w.series[name], v, w.window)

		// A partial window is not a short window: fitting one would let a counter fire
		// on the first four samples of a hundred-sample window an operator chose
		// precisely because they wanted a hundred.
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

		// A counter that just fired starts a fresh window. Without this a leak that is
		// still leaking fires on every subsequent sample under Repeat, and the evidence
		// of one leak fills the disk.
		w.series[name] = nil
		w.fired[name]++

		f.Counter = name
		f.Window = append([]Sample(nil), w.samples...)
		w.report(f)
	}
}

// report writes the dump and logs the finding. A dump that fails is still logged,
// because the counter and the slope are the two facts an operator needs first, and
// a full disk is the likeliest reason the dump failed.
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
//
// Three conditions must hold, and each rejects a shape the others admit:
//
//   - Staircase: the window splits into three blocks, and each block sits entirely
//     above the one before it. This is what "sustained" means here. It rejects a
//     sawtooth, a single spike, a fan-out that returns to baseline, and a step that
//     rises once and settles (a slope alone accepts every one of them, and two
//     blocks accept the step). Unlike strict monotonicity it survives the ordinary
//     rise and fall of a live heap between collections, because a sample is only
//     ever compared against a different block, never against its neighbour.
//   - Slope: the least-squares fit over the window is at least MinSlope, so growth
//     too slow to matter over the life of a process is not called a leak.
//   - Delta: the window's last sample is at least MinDelta above its first, so growth
//     too small to matter at all is not called a leak.
//
// Growth that pauses inside the window is not reported, and that is the intended
// trade: the next window catches it if it is real, and one deferred window is a far
// smaller cost than the false positive that gets --leak-watch turned off.
//
// The returned Finding carries no counter name and no window; push fills those in.
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

// staircase reports whether the window rises in three separated blocks: every
// sample of the middle block above every sample of the first, and every sample of
// the last block above every sample of the middle.
//
// Three blocks rather than two, because two halves cannot tell a ramp from a step:
// a counter that jumps once and then holds a new flat level clears any two-way
// separation, and a step is a fan-out that raised the floor, not a leak. A leak has
// to still be climbing in the second half of the evidence.
//
// The outer blocks take the floor of n/3 samples each and the middle takes the rest,
// so a window of four compares one sample against two against one.
func staircase(vs []float64) bool {
	third := len(vs) / 3
	if third == 0 {
		return false
	}
	first, middle, last := vs[:third], vs[third:len(vs)-third], vs[len(vs)-third:]
	return above(middle, first) && above(last, middle)
}

// above reports whether every value in hi exceeds every value in lo.
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

// leastSquaresSlope fits vs against its own indices and returns the slope, in
// counter units per sample. The x values are 0..n-1, so the denominator is a
// constant of n and the whole fit is one pass.
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

// counterValue reads a named counter off a sample. It reports false for a counter
// the platform could not measure (Unknown) and for a name no sample carries, so the
// detector can tell a gap from a zero. A leak detector that reads "not measurable
// here" as zero invents a slope out of the platform's silence.
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

// appendCapped appends v and keeps only the newest n elements, reusing the backing
// array: the sampler runs for the life of the process, and a window that reallocates
// on every sample is a leak in the leak detector. The first append allocates the
// whole window, so append's own doubling never gives it a capacity above n.
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

// dumpLeak writes the evidence for one finding into the bundle: the labelled
// goroutine profile that names the action responsible, the human-readable stacks,
// the heap profile, and the window of timeline samples that fired. It returns the
// member names it wrote.
//
// It does not force a garbage collection first. The heap profile already describes
// the last completed collection, and forcing one here would change the behaviour of
// the process the watchdog is meant to observe.
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

	// debug=1 folds identical stacks into counts and prints each stack's pprof
	// labels. It is the member that answers "which action left these goroutines
	// parked", and on a leak that is the only question worth asking first.
	write(prefix+"goroutine.labels.txt", func(w io.Writer) error { return pprof.Lookup("goroutine").WriteTo(w, 1) })
	write(prefix+"goroutine.txt", func(w io.Writer) error { return pprof.Lookup("goroutine").WriteTo(w, 2) })
	write(prefix+MemberHeap, func(w io.Writer) error { return pprof.Lookup("heap").WriteTo(w, 0) })
	write(prefix+"window.jsonl", func(w io.Writer) error { return writeSamples(w, f.Window) })

	return names, errors.Join(errs...)
}

// leakSeq numbers a counter's dumps, so a Repeat watchdog does not overwrite the
// first firing (the interesting one) with the tenth.
func (b *Bundle) leakSeq(counter string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.leakDumps[counter]
	b.leakDumps[counter] = n + 1
	return n
}

// writeAtomic writes a bundle member through fsatomic, so a reader watching the
// bundle directory (an operator tailing it, a CI step collecting it) never opens a
// profile that is still being written, and a bundle collected from a machine that
// then died is still on disk. Profiles are streamed rather than assembled in memory:
// a goroutine dump from the process a leak watchdog just fired on is exactly the case
// where holding the whole member as a byte slice is a bad idea.
func (b *Bundle) writeAtomic(name string, fn func(io.Writer) error) error {
	if err := fsatomic.WriteStream(filepath.Join(b.cfg.Dir, name), 0o600, fn); err != nil {
		return fmt.Errorf("diag: write %s: %w", name, err)
	}
	return nil
}
