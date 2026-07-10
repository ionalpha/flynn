// Package diag captures a runtime profile bundle from a running flynn process.
//
// Where observe carries agent semantics (what a run did, how much it cost), diag
// carries process runtime facts: where CPU went, what the heap held, which
// goroutines were alive, how many file descriptors and child processes the
// process owned over time. The two are deliberately separate: an operator reading
// a stuck run needs the second kind of evidence, and it must be capturable from a
// binary that is already built and already misbehaving.
//
// A bundle is a directory of pprof profiles, a JSONL timeline, and a manifest that
// names and hashes every member, so a bundle can be moved off the machine and
// still be shown intact.
//
// A bundle can also watch itself. With Config.Leak set, the timeline sampler fits
// a growth slope across every counter it records and writes a labelled goroutine
// profile, a heap profile, and the offending window into the bundle at the moment
// growth starts, unattended. See leak.go.
//
// Nothing here runs unless a bundle is explicitly started: there is no init, no
// background goroutine, and no allocation on the disabled path. Start with an
// empty Config returns a nil *Bundle, and every method on a nil *Bundle is a
// no-op, so the caller never branches on whether profiling is on.
//
// The package depends only on the standard library, the clock port (so the
// timeline sampler is driven by an injected clock and stays testable), the secret
// port (so an argv recorded in the manifest is redacted by the same redactor the
// rest of the agent uses), and the observe port (so a leak the watchdog finds is
// reported through the same logger as everything else).
package diag

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/version"
)

// Environment variables that turn a bundle on without changing the command line,
// so a hosted or containerized instance can be profiled in place.
const (
	// EnvDir names the bundle directory, equivalent to --profile.
	EnvDir = "FLYNN_PROFILE"
	// EnvContention enables the block and mutex profiles, equivalent to
	// --profile-contention. Any value Go's strconv.ParseBool accepts as true enables it.
	EnvContention = "FLYNN_PROFILE_CONTENTION"
)

// DefaultInterval is how often the timeline sampler records a line when Config
// leaves Interval unset. One second is fine enough to fit a growth slope over a
// run of any interesting length and coarse enough that the sampler's own cost
// stays in the noise.
const DefaultInterval = time.Second

// Bundle member file names. A reader (see the `flynn diagnose` command) resolves
// members by these names, so they are part of the bundle's contract.
const (
	MemberCPU          = "cpu.pprof"
	MemberHeap         = "heap.pprof"
	MemberAllocs       = "allocs.pprof"
	MemberGoroutine    = "goroutine.pprof"
	MemberGoroutineTxt = "goroutine.txt"
	MemberGoroutineLbl = "goroutine.labels.txt"
	MemberThreadcreate = "threadcreate.pprof"
	MemberBlock        = "block.pprof"
	MemberMutex        = "mutex.pprof"
	MemberTimeline     = "runtime.jsonl"
	MemberManifest     = "manifest.json"
)

// Config describes a bundle to capture. The zero Config is disabled: Start
// returns a nil Bundle and touches nothing.
type Config struct {
	// Dir is the bundle directory, created if absent. An empty Dir disables capture.
	Dir string

	// Contention enables the block and mutex profiles. Both make the runtime record
	// every blocking event and every contended lock handoff, which costs real time in
	// a hot process, so they are off unless asked for.
	Contention bool

	// Interval is the timeline sampler's period. Zero means DefaultInterval. A
	// negative Interval disables the sampler, leaving the rest of the bundle intact.
	Interval time.Duration

	// Args is the command line to record in the manifest, redacted. Callers pass
	// os.Args. An empty Args records no command line.
	Args []string

	// Clock is the time source for the timeline and the manifest's start and end
	// stamps. Nil means clock.System. A test supplies a clock.Manual to drive the
	// sampler deterministically.
	Clock clock.Timing

	// Leak turns the leak watchdog on. Nil is the disabled watchdog. The watchdog
	// rides the timeline sampler, so it needs one: a Config with a Leak and a
	// negative Interval is an error rather than a silently inert watchdog.
	Leak *LeakConfig

	// Counters are application-supplied gauges sampled into every timeline line
	// alongside the built-in ones, and watched by the watchdog when Leak's thresholds
	// name them. This is where a counter this package cannot know about (unremoved
	// temporary directories, the event log's size on disk) is registered.
	Counters []Counter

	// sampled, when non-nil, receives once after every timeline sample. It is
	// unexported because only this package's tests set it: it lets a test step a
	// Manual clock one tick at a time without racing the sampler's timer re-arm.
	sampled chan struct{}
}

// FromEnv fills the parts of cfg the caller left unset from the environment, so
// FLYNN_PROFILE turns capture on for a process whose command line cannot be
// changed. An explicit Dir or Contention already set on cfg wins.
func FromEnv(cfg Config) Config {
	if cfg.Dir == "" {
		cfg.Dir = strings.TrimSpace(os.Getenv(EnvDir))
	}
	if !cfg.Contention {
		if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(EnvContention))); err == nil {
			cfg.Contention = v
		}
	}
	if cfg.Leak == nil {
		if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(EnvLeakWatch))); err == nil && v {
			cfg.Leak = &LeakConfig{}
		}
	}
	return cfg
}

// Bundle is an open profile capture. A nil *Bundle is the disabled bundle: every
// method on it is a no-op returning nil, so a caller holds one unconditionally.
//
// A Bundle is written by Stop, which is not safe to call concurrently with itself;
// call it once, from the same goroutine that owns the command's exit path.
type Bundle struct {
	cfg     Config
	clk     clock.Timing
	id      string
	started time.Time

	cpuFile  *os.File
	timeline *timelineWriter

	mu          sync.Mutex
	annotations map[string]string
	leakDumps   map[string]int
	stopped     bool
}

// Start opens the bundle directory and begins capture. It returns a nil Bundle
// and a nil error when cfg.Dir is empty, which is the disabled path: no directory
// is touched, no goroutine starts, and nothing is allocated.
//
// The caller stops the bundle exactly once, normally from a defer on the process's
// single exit path. A bundle whose process calls os.Exit is never written, because
// os.Exit runs no defers; that is a property of os.Exit, not of this package.
func Start(cfg Config) (*Bundle, error) {
	if cfg.Dir == "" {
		return nil, nil
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}

	// A watchdog with no sampler would never see a sample. Say so here rather than
	// let an operator run a week-long soak that was never watching anything.
	if cfg.Leak != nil && cfg.Interval < 0 {
		return nil, errors.New("diag: leak watch needs the timeline sampler, but Interval disables it")
	}

	// The watchdog is built before anything is created, so a rejected threshold costs
	// no bundle directory, no CPU profile, and nothing for a reader to half-trust. Its
	// dump target is wired below, once there is a bundle to dump into.
	var wd *watchdog
	if cfg.Leak != nil {
		var err error
		if wd, err = newWatchdog(*cfg.Leak, nil); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("diag: create bundle dir: %w", err)
	}

	b := &Bundle{
		cfg:       cfg,
		clk:       cfg.Clock,
		id:        ids.New(),
		started:   cfg.Clock.Now(),
		leakDumps: make(map[string]int),
	}

	var observer func(Sample)
	if wd != nil {
		wd.dump = b.dumpLeak
		observer = wd.push
	}

	cpu, err := b.create(MemberCPU)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(cpu); err != nil {
		_ = cpu.Close()
		return nil, fmt.Errorf("diag: start cpu profile: %w", err)
	}
	b.cpuFile = cpu

	// Contention profiling is opt-in because both rates cost time in every blocking
	// operation and every contended lock handoff for the life of the process.
	if cfg.Contention {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
	}

	if cfg.Interval > 0 {
		tl, err := b.startTimeline(observer)
		if err != nil {
			pprof.StopCPUProfile()
			_ = cpu.Close()
			return nil, err
		}
		b.timeline = tl
	}

	// Labelling is live only while a bundle is open, so the waist and the long-lived
	// goroutine loops pay nothing for it in an unprofiled process. Set it last: every
	// failure above returns without a bundle, and a process with no bundle must not
	// be paying for labels.
	profiling.Store(true)

	return b, nil
}

// Annotate records a key/value pair in the manifest, so a caller can correlate a
// bundle with the run it captured once the run's identity is known. It is safe on
// a nil Bundle and safe for concurrent use.
func (b *Bundle) Annotate(key, value string) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.annotations == nil {
		b.annotations = make(map[string]string, 4)
	}
	b.annotations[key] = value
}

// Dir returns the bundle directory, or "" for a disabled bundle.
func (b *Bundle) Dir() string {
	if b == nil {
		return ""
	}
	return b.cfg.Dir
}

// ID returns the bundle's unique identifier, or "" for a disabled bundle.
func (b *Bundle) ID() string {
	if b == nil {
		return ""
	}
	return b.id
}

// Stop ends capture, writes the exit-time profiles and the manifest, and closes
// every member. It is a no-op on a nil Bundle, and a second call is a no-op.
//
// Stop reports the first error it hit but always attempts every remaining member,
// so a bundle is as complete as the failure allows rather than truncated at it.
func (b *Bundle) Stop() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	b.mu.Unlock()

	// Nothing after this point can be labelled into a profile that is already being
	// written, and the process may keep running after its bundle is sealed.
	profiling.Store(false)

	var errs []error

	// Stop the sampler before the exit-time profiles so the timeline's last line
	// describes the process as the profiles below see it, not a state it left behind.
	if b.timeline != nil {
		errs = append(errs, b.timeline.stop())
	}

	pprof.StopCPUProfile()
	if err := b.cpuFile.Close(); err != nil {
		errs = append(errs, fmt.Errorf("diag: close %s: %w", MemberCPU, err))
	}

	// A GC before the heap profile makes the in-use graph describe what is actually
	// reachable, rather than what has not been collected yet.
	runtime.GC()

	errs = append(
		errs,
		b.writeProfile(MemberHeap, "heap", 0),
		b.writeProfile(MemberAllocs, "allocs", 0),
		b.writeProfile(MemberGoroutine, "goroutine", 0),
		// debug=2 renders every goroutine's full stack as text. It is the member a
		// human reads first on a hang, and no pprof tooling is needed to read it.
		b.writeProfile(MemberGoroutineTxt, "goroutine", 2),
		// debug=1 folds identical stacks into counts and prints the pprof labels
		// each carries, which debug=2 omits. It is the member that answers "which
		// action left 1,900 goroutines parked", again with no pprof tooling.
		b.writeProfile(MemberGoroutineLbl, "goroutine", 1),
		b.writeProfile(MemberThreadcreate, "threadcreate", 0),
	)

	if b.cfg.Contention {
		errs = append(
			errs,
			b.writeProfile(MemberBlock, "block", 0),
			b.writeProfile(MemberMutex, "mutex", 0),
		)
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	}

	errs = append(errs, b.writeManifest())

	return errors.Join(errs...)
}

// create opens a bundle member for writing, owner-readable only: a bundle holds a
// process's stacks and a redacted command line, not a world-readable artifact. The
// name is always a member constant, joined onto the directory the operator named on
// their own command line, so no untrusted input reaches the path.
func (b *Bundle) create(name string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(b.cfg.Dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: a constant member name under an operator-chosen directory
	if err != nil {
		return nil, fmt.Errorf("diag: create %s: %w", name, err)
	}
	return f, nil
}

// writeProfile writes the named runtime profile to a bundle member at the given
// debug level. A profile the runtime does not know is an internal error, not a
// missing member, so it is reported rather than skipped silently.
func (b *Bundle) writeProfile(member, profile string, debug int) error {
	p := pprof.Lookup(profile)
	if p == nil {
		return fmt.Errorf("diag: no such runtime profile %q", profile)
	}
	f, err := b.create(member)
	if err != nil {
		return err
	}
	if err := p.WriteTo(f, debug); err != nil {
		_ = f.Close()
		return fmt.Errorf("diag: write %s: %w", member, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("diag: close %s: %w", member, err)
	}
	return nil
}

// writeManifest describes the bundle and hashes every other member, so a bundle
// that has been copied, archived, or partially truncated can be shown to be intact
// before anyone reasons from its contents. The manifest is written last and is the
// only member it does not hash.
func (b *Bundle) writeManifest() error {
	b.mu.Lock()
	annotations := make(map[string]string, len(b.annotations))
	for k, v := range b.annotations {
		annotations[k] = v
	}
	b.mu.Unlock()
	if len(annotations) == 0 {
		annotations = nil
	}

	members, err := hashMembers(b.cfg.Dir, MemberManifest)
	if err != nil {
		return err
	}

	m := Manifest{
		BundleID:         b.id,
		FlynnVersion:     version.String(),
		Revision:         version.Revision(),
		GoVersion:        runtime.Version(),
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		NumCPU:           runtime.NumCPU(),
		Args:             RedactArgs(b.cfg.Args),
		Contention:       b.cfg.Contention,
		SampleIntervalMs: b.cfg.Interval.Milliseconds(),
		StartedAt:        b.started,
		EndedAt:          b.clk.Now(),
		Annotations:      annotations,
		Members:          members,
	}

	f, err := b.create(MemberManifest)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		_ = f.Close()
		return fmt.Errorf("diag: write %s: %w", MemberManifest, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("diag: close %s: %w", MemberManifest, err)
	}
	return nil
}
