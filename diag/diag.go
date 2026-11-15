// Package diag captures a runtime profile bundle from a running flynn process: where
// CPU went, what the heap held, which goroutines were alive, and how file
// descriptors and child processes grew over time. Package observe records what a
// run did; diag records what the process did, and works on a binary that is already
// built and already misbehaving.
//
// A bundle is a directory of pprof profiles, a JSONL timeline and a manifest that
// hashes every member, so it can leave the machine and still be shown intact. With
// Config.Leak set, the sampler fits a growth slope to every counter and dumps
// labelled goroutine and heap profiles when growth starts.
//
// Nothing runs until a bundle is started: no init, no goroutine, no allocation on
// the disabled path. Start with an empty Config returns a nil *Bundle, and every
// method on a nil *Bundle is a no-op, so callers never branch on whether profiling
// is on.
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

// Environment variables that turn a bundle on without changing the command line, so
// a hosted or containerized instance can be profiled in place.
const (
	// EnvDir names the bundle directory, equivalent to --profile.
	EnvDir = "FLYNN_PROFILE"
	// EnvContention enables the block and mutex profiles, equivalent to
	// --profile-contention. Any value strconv.ParseBool reads as true enables it.
	EnvContention = "FLYNN_PROFILE_CONTENTION"
)

// DefaultInterval is the timeline period when Config leaves Interval unset: fine
// enough to fit a growth slope over any run worth watching, coarse enough that the
// sampler's own cost stays in the noise.
const DefaultInterval = time.Second

// Bundle member file names. `flynn diagnose` resolves members by these names, so they
// are part of the bundle's contract.
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

// Config describes a bundle to capture. The zero Config is disabled.
type Config struct {
	// Dir is the bundle directory, created if absent. An empty Dir disables capture.
	Dir string

	// Contention enables the block and mutex profiles. Both record every blocking event
	// and contended lock handoff, which costs time in a hot process, so they are opt-in.
	Contention bool

	// Interval is the timeline period; zero means DefaultInterval. A negative Interval
	// disables the sampler and keeps the rest of the bundle.
	Interval time.Duration

	// Args is the command line to record, redacted, in the manifest; normally os.Args.
	Args []string

	// Clock times the timeline and the manifest's start and end stamps; nil means
	// clock.System. A clock.Manual drives the sampler deterministically in tests.
	Clock clock.Timing

	// Leak turns the leak watchdog on; nil disables it. The watchdog rides the timeline
	// sampler, so a Leak with a negative Interval is an error.
	Leak *LeakConfig

	// Counters are application gauges sampled into every timeline line beside the
	// built-in ones and watched when Leak's thresholds name them, for facts this package
	// cannot see (unremoved temp directories, the event log's size on disk).
	Counters []Counter

	// Children reports how many child processes the application has started and not
	// reaped, filling the timeline's child_procs. It is read on the sampler goroutine
	// once per interval and must not block.
	//
	// This package does not count children itself: walking /proc or a Windows process
	// snapshot costs a read per process on every sample, and pid reuse can make a
	// stranger name this pid as its parent. Only the spawners know what they spawned;
	// see package procs. Nil records Unknown rather than zero, which restarts the
	// watchdog's window instead of feeding it false zeroes.
	Children func() int

	// sampled, when non-nil, receives after every timeline sample, so a test can step a
	// Manual clock one tick at a time without racing the sampler's timer re-arm.
	sampled chan struct{}
}

// FromEnv fills what cfg left unset from the environment, so FLYNN_PROFILE turns
// capture on for a process whose command line cannot change. A Dir or Contention
// already set on cfg wins.
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
// method is a no-op returning nil. Stop is not safe to call concurrently with
// itself; call it once, from the goroutine that owns the exit path.
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

// Start opens the bundle directory and begins capture. With cfg.Dir empty it returns
// a nil Bundle and nil error, touching nothing. Stop it once, normally from a defer on
// the process's single exit path; a process that calls os.Exit runs no defers, so its
// bundle is never written.
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

	wd, err := validateStart(cfg)
	if err != nil {
		return nil, err
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

	if err := b.startCapture(observer); err != nil {
		return nil, err
	}

	// Enable labelling last: every failure above returns without a bundle, and a
	// process with no bundle must not pay for labels.
	profiling.Store(true)

	return b, nil
}

// validateStart rejects an unworkable Config before anything is created, so a bad
// one leaves no directory and no half-written bundle. It returns the watchdog when
// cfg.Leak asks for one; Start wires its dump target.
func validateStart(cfg Config) (*watchdog, error) {
	// A watchdog with no sampler would never see a sample; fail now rather than after a
	// week-long soak that watched nothing.
	if cfg.Leak != nil && cfg.Interval < 0 {
		return nil, errors.New("diag: leak watch needs the timeline sampler, but Interval disables it")
	}

	// A nil Read panics at the baseline sample, and the name becomes part of a leak dump
	// path (leak.<name>.<seq>.<member>): a slash writes into a missing directory and
	// ".." escapes the bundle.
	for _, c := range cfg.Counters {
		if c.Read == nil {
			return nil, fmt.Errorf("diag: counter %q has no Read function", c.Name)
		}
		if !safeCounterName(c.Name) {
			return nil, fmt.Errorf("diag: counter name %q must be a plain filename component: it becomes part of a leak dump path", c.Name)
		}
	}

	if cfg.Leak == nil {
		return nil, nil
	}
	return newWatchdog(*cfg.Leak, nil)
}

// startCapture starts the CPU profile, opt-in contention profiling and the timeline
// sampler, in that order. On failure it stops what it started, so no profile runs
// behind an error.
func (b *Bundle) startCapture(observer func(Sample)) error {
	cpu, err := b.create(MemberCPU)
	if err != nil {
		return err
	}
	if err := pprof.StartCPUProfile(cpu); err != nil {
		_ = cpu.Close()
		return fmt.Errorf("diag: start cpu profile: %w", err)
	}
	b.cpuFile = cpu

	if b.cfg.Contention {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
	}

	if b.cfg.Interval > 0 {
		tl, err := b.startTimeline(observer)
		if err != nil {
			pprof.StopCPUProfile()
			_ = cpu.Close()
			return err
		}
		b.timeline = tl
	}
	return nil
}

// Annotate records a key/value pair in the manifest, so a bundle can be matched to
// its run once the run's identity is known. It is safe on a nil Bundle and for
// concurrent use.
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

// Stop ends capture, writes the exit-time profiles and the manifest, and closes every
// member; a nil Bundle or a second call is a no-op. It returns the first error but
// still writes every remaining member, so a failure costs only what it touched.
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

	// Nothing may be labelled into a profile being written, and the process may keep
	// running after its bundle is sealed.
	profiling.Store(false)

	var errs []error

	// Stop the sampler first so the timeline's last line matches the state the exit
	// profiles see.
	if b.timeline != nil {
		errs = append(errs, b.timeline.stop())
	}

	pprof.StopCPUProfile()
	if err := b.cpuFile.Close(); err != nil {
		errs = append(errs, fmt.Errorf("diag: close %s: %w", MemberCPU, err))
	}

	// GC first so the heap profile shows what is reachable, not what is uncollected.
	runtime.GC()

	errs = append(
		errs,
		b.writeProfile(MemberHeap, "heap", 0),
		b.writeProfile(MemberAllocs, "allocs", 0),
		b.writeProfile(MemberGoroutine, "goroutine", 0),
		// debug=2: every goroutine's full stack as text, the member read first on a hang.
		b.writeProfile(MemberGoroutineTxt, "goroutine", 2),
		// debug=1: identical stacks folded into counts with their pprof labels, which
		// debug=2 omits. It answers "which action left 1,900 goroutines parked".
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

// create opens a bundle member for writing, owner-readable only, since it holds the
// process's stacks and command line. name is always a member constant under the
// operator's chosen directory, so no untrusted input reaches the path.
func (b *Bundle) create(name string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(b.cfg.Dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: a constant member name under an operator-chosen directory
	if err != nil {
		return nil, fmt.Errorf("diag: create %s: %w", name, err)
	}
	return f, nil
}

// writeProfile writes the named runtime profile at the given debug level. An
// unknown profile is an internal error, so it is reported rather than skipped.
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

// writeManifest describes the bundle and hashes every other member, so a copied,
// archived or truncated bundle can be checked before anyone trusts it. It is written
// last and is the only unhashed member.
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
