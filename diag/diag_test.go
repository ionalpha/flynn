package diag

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/secret"
)

// epoch is the fixed start time every Manual-clock test runs from.
var epoch = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// These tests must not run in parallel with one another: the CPU, block, and mutex
// profiles are process-global, and a second StartCPUProfile while one is running
// fails. That is a property of the runtime, not of this package.

func TestStartDisabledDoesNothing(t *testing.T) {
	dir := t.TempDir()

	b, err := Start(Config{Dir: ""})
	if err != nil {
		t.Fatalf("Start with no dir: %v", err)
	}
	if b != nil {
		t.Fatalf("Start with no dir returned %v, want a nil bundle", b)
	}

	// Every method is safe on the disabled bundle, so no caller branches on it.
	b.Annotate("run", "r1")
	if got := b.Dir(); got != "" {
		t.Errorf("nil bundle Dir() = %q, want empty", got)
	}
	if got := b.ID(); got != "" {
		t.Errorf("nil bundle ID() = %q, want empty", got)
	}
	if err := b.Stop(); err != nil {
		t.Errorf("nil bundle Stop() = %v, want nil", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("disabled bundle wrote %d files, want none", len(entries))
	}
}

func TestBundleWritesEveryMemberAndAHashedManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle") // not pre-created: Start must create it
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{})

	b, err := Start(Config{
		Dir:      dir,
		Interval: time.Second,
		Clock:    clk,
		Args:     []string{"flynn", "goal", "ship the thing"},
		sampled:  sampled,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if b.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", b.Dir(), dir)
	}
	if b.ID() == "" {
		t.Error("ID() is empty, want a bundle id")
	}
	b.Annotate("run", "run-123")

	const ticks = 3
	for range ticks {
		clk.Advance(time.Second)
		<-sampled
	}

	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Contention was off, so neither contention member exists.
	want := []string{MemberCPU, MemberHeap, MemberAllocs, MemberGoroutine, MemberGoroutineTxt, MemberThreadcreate, MemberTimeline, MemberManifest}
	for _, name := range want {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("member %s missing: %v", name, err)
		}
	}
	for _, name := range []string{MemberBlock, MemberMutex} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("member %s written without --profile-contention", name)
		}
	}

	// Every pprof protobuf member is gzip-framed, which is what `go tool pprof`
	// requires to open it. The text member is the human-readable goroutine dump.
	for _, name := range []string{MemberCPU, MemberHeap, MemberAllocs, MemberGoroutine, MemberThreadcreate} {
		data := readFile(t, dir, name)
		if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
			t.Errorf("member %s is not a gzip-framed pprof profile", name)
		}
	}
	// debug=2 renders raw stacks ("goroutine 1 [running]:"), not the debug=1 summary
	// header. That is the point: it is readable with no pprof tooling at all.
	txt := string(readFile(t, dir, MemberGoroutineTxt))
	if !strings.HasPrefix(txt, "goroutine ") || !strings.Contains(txt, "[running]") {
		t.Errorf("%s is not a full goroutine stack dump, got %.60q", MemberGoroutineTxt, txt)
	}

	m := readManifest(t, dir)
	if m.BundleID != b.ID() {
		t.Errorf("manifest bundle_id = %q, want %q", m.BundleID, b.ID())
	}
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH {
		t.Errorf("manifest platform = %s/%s, want %s/%s", m.OS, m.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if m.GoVersion != runtime.Version() {
		t.Errorf("manifest go_version = %q, want %q", m.GoVersion, runtime.Version())
	}
	if m.Contention {
		t.Error("manifest reports contention profiling, which was off")
	}
	if m.SampleIntervalMs != 1000 {
		t.Errorf("manifest sample_interval_ms = %d, want 1000", m.SampleIntervalMs)
	}
	if !m.StartedAt.Equal(epoch) {
		t.Errorf("manifest started_at = %v, want %v", m.StartedAt, epoch)
	}
	if want := epoch.Add(ticks * time.Second); !m.EndedAt.Equal(want) {
		t.Errorf("manifest ended_at = %v, want %v (the clock never advanced past the last tick)", m.EndedAt, want)
	}
	if m.Annotations["run"] != "run-123" {
		t.Errorf("manifest annotations = %v, want run=run-123", m.Annotations)
	}
	// The objective is free user text and never reaches the manifest.
	if got := strings.Join(m.Args, " "); strings.Contains(got, "ship the thing") {
		t.Errorf("manifest args leak the objective: %q", got)
	}

	// The manifest hashes every other member, and nothing else.
	if len(m.Members) != len(want)-1 {
		t.Errorf("manifest lists %d members, want %d (all but the manifest itself)", len(m.Members), len(want)-1)
	}
	for _, mem := range m.Members {
		if mem.Name == MemberManifest {
			t.Error("manifest lists itself as a member")
		}
		data := readFile(t, dir, mem.Name)
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != mem.SHA256 {
			t.Errorf("member %s: manifest hash %s, actual %s", mem.Name, mem.SHA256, got)
		}
		if int64(len(data)) != mem.Bytes {
			t.Errorf("member %s: manifest size %d, actual %d", mem.Name, mem.Bytes, len(data))
		}
	}
}

// The timeline is the series D3 fits a growth slope over, so its shape is a
// contract: a baseline line at capture start, one line per tick, and a final line
// taken as the exit-time profiles are written.
func TestTimelineWritesOneLinePerTickPlusBaselineAndFinal(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{})

	b, err := Start(Config{Dir: dir, Interval: 250 * time.Millisecond, Clock: clk, sampled: sampled})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	const ticks = 4
	for range ticks {
		clk.Advance(250 * time.Millisecond)
		<-sampled
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	samples := readTimeline(t, dir)
	if len(samples) != ticks+2 {
		t.Fatalf("timeline has %d samples, want %d (baseline + %d ticks + final)", len(samples), ticks+2, ticks)
	}

	// Baseline at t0; one sample per tick; the final sample at the last tick's time,
	// because the Manual clock never moved past it.
	for i, s := range samples[:ticks+1] {
		want := epoch.Add(time.Duration(i) * 250 * time.Millisecond)
		if !s.T.Equal(want) {
			t.Errorf("sample %d at %v, want %v", i, s.T, want)
		}
	}
	if last := samples[ticks+1]; !last.T.Equal(epoch.Add(ticks * 250 * time.Millisecond)) {
		t.Errorf("final sample at %v, want the last tick's time", last.T)
	}

	for i, s := range samples {
		if s.Goroutines < 1 {
			t.Errorf("sample %d reports %d goroutines, want at least this one", i, s.Goroutines)
		}
		if s.HeapAllocBytes == 0 {
			t.Errorf("sample %d reports a zero live heap", i)
		}
		if s.OpenFDs != Unknown && s.OpenFDs < 1 {
			t.Errorf("sample %d reports %d open fds; a running process holds at least one", i, s.OpenFDs)
		}
		if s.ChildProcs != Unknown && s.ChildProcs < 0 {
			t.Errorf("sample %d reports %d child procs", i, s.ChildProcs)
		}
	}
}

// A negative interval keeps the rest of the bundle and drops the sampler, so a
// caller that only wants profiles pays nothing for a stop-the-world read.
func TestNegativeIntervalDisablesTheSamplerOnly(t *testing.T) {
	dir := t.TempDir()

	b, err := Start(Config{Dir: dir, Interval: -1, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, MemberTimeline)); err == nil {
		t.Error("timeline written despite a negative interval")
	}
	if _, err := os.Stat(filepath.Join(dir, MemberCPU)); err != nil {
		t.Errorf("cpu profile missing: %v", err)
	}
}

func TestContentionAddsBlockAndMutexAndRestoresTheRates(t *testing.T) {
	dir := t.TempDir()

	b, err := Start(Config{Dir: dir, Contention: true, Interval: -1, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := runtime.SetMutexProfileFraction(-1); got != 1 {
		t.Errorf("mutex profile fraction during capture = %d, want 1", got)
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	for _, name := range []string{MemberBlock, MemberMutex} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("member %s missing under --profile-contention: %v", name, err)
		}
	}
	// The rates are process-global and cost every blocking operation, so a stopped
	// bundle must leave them off for whatever runs next.
	if got := runtime.SetMutexProfileFraction(-1); got != 0 {
		t.Errorf("mutex profile fraction after Stop = %d, want 0", got)
	}
	if !readManifest(t, dir).Contention {
		t.Error("manifest does not record that contention profiling was on")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	b, err := Start(Config{Dir: dir, Interval: -1, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	// A second Stop must not stop a CPU profile it no longer owns, nor rewrite members.
	if err := b.Stop(); err != nil {
		t.Errorf("second Stop: %v, want nil", err)
	}
}

func TestStartRejectsAnUnusableBundleDir(t *testing.T) {
	// A regular file cannot become a directory, so Start fails rather than capturing
	// into nowhere. It must leave no CPU profile running behind it.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Start(Config{Dir: filepath.Join(file, "bundle")}); err == nil {
		t.Fatal("Start into a path under a regular file succeeded, want an error")
	}

	// Proof the failed Start left no CPU profile running: a fresh one can start.
	b, err := Start(Config{Dir: t.TempDir(), Interval: -1, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("Start after a failed Start: %v (the failed Start leaked a running CPU profile)", err)
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestFromEnvFillsUnsetFieldsOnly(t *testing.T) {
	t.Setenv(EnvDir, "/tmp/from-env")
	t.Setenv(EnvContention, "true")

	got := FromEnv(Config{})
	if got.Dir != "/tmp/from-env" {
		t.Errorf("Dir = %q, want the environment's", got.Dir)
	}
	if !got.Contention {
		t.Error("Contention = false, want the environment's true")
	}

	// An explicit flag wins over the environment.
	got = FromEnv(Config{Dir: "/tmp/explicit"})
	if got.Dir != "/tmp/explicit" {
		t.Errorf("Dir = %q, want the explicit value to win", got.Dir)
	}
}

func TestFromEnvIgnoresAnUnparseableContentionValue(t *testing.T) {
	t.Setenv(EnvContention, "yes please")
	if FromEnv(Config{}).Contention {
		t.Error("an unparseable FLYNN_PROFILE_CONTENTION turned contention profiling on")
	}
}

// --- helpers -----------------------------------------------------------------

func readFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func readManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(readFile(t, dir, MemberManifest), &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

func readTimeline(t *testing.T, dir string) []Sample {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, MemberTimeline))
	if err != nil {
		t.Fatalf("open timeline: %v", err)
	}
	defer func() { _ = f.Close() }()

	var samples []Sample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("parse timeline line %q: %v", sc.Text(), err)
		}
		samples = append(samples, s)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan timeline: %v", err)
	}
	return samples
}

// The redactor is secret's, not a second one grown here.
func TestRedactionUsesTheSecretPackagesRendering(t *testing.T) {
	got := RedactArgs([]string{"flynn", "goal", "something private"})
	if got[2] != secret.Redacted {
		t.Errorf("redacted objective rendered as %q, want secret.Redacted (%q)", got[2], secret.Redacted)
	}
}
