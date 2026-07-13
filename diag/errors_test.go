package diag

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// The failure paths of a capture. A bundle is written by a process that is already
// misbehaving, so every one of these is a path a real operator can reach: a full
// disk, a bundle directory an earlier run left something in, a member that cannot
// be opened. What each test pins is that the failure is reported with the member it
// happened on, that nothing half-written is left for a reader to trust, and that a
// failed Start leaves no CPU profile running behind it.
//
// The faults are injected by making a member path unopenable (a directory where a
// file belongs, which fails identically on Windows and on POSIX) or by handing the
// writer a failing io.Writer, rather than by changing permissions: a directory mode
// does not stop a write on Windows, so a chmod-based test would pass by accident.

// errSentinel is the injected write failure every fault-injecting test below
// asserts on, so a passing test proves the error it forced is the error that came
// back.
var errSentinel = errors.New("injected write failure")

// failWriter fails on its nth write (1-based) and passes every other write through
// to inner. The shared testkit injectors cannot be used here: testkit reaches this
// package through the bus, so importing it from an internal diag test is a cycle.
type failWriter struct {
	inner  io.Writer
	failOn int // 0 fails every write
	writes int
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failOn == 0 || w.writes == w.failOn {
		return 0, errSentinel
	}
	return w.inner.Write(p)
}

// mkdirMember creates a directory where a bundle member file belongs, so opening
// that member for writing fails.
func mkdirMember(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestStartFailsWhenTheCPUMemberCannotBeCreated: the bundle directory exists but
// its cpu.pprof is a directory, so the first member cannot be opened. Start reports
// the member it failed on and leaves no CPU profile running, which the following
// Start proves by succeeding.
func TestStartFailsWhenTheCPUMemberCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	mkdirMember(t, dir, MemberCPU)

	_, err := Start(Config{Dir: dir, Interval: -1, Clock: clock.NewManual(epoch)})
	if err == nil {
		t.Fatal("Start succeeded with an unopenable cpu member")
	}
	if !strings.Contains(err.Error(), MemberCPU) {
		t.Errorf("error %q does not name the member it failed on", err)
	}

	b, err := Start(Config{Dir: t.TempDir(), Interval: -1, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("Start after a failed Start: %v (the failed Start left a CPU profile running)", err)
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
}

// TestStartFailsWhenTheTimelineMemberCannotBeCreated: the sampler is started after
// the CPU profile, so its failure has to unwind one. A later Start proves it did.
func TestStartFailsWhenTheTimelineMemberCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	mkdirMember(t, dir, MemberTimeline)

	_, err := Start(Config{Dir: dir, Interval: time.Second, Clock: clock.NewManual(epoch)})
	if err == nil {
		t.Fatal("Start succeeded with an unopenable timeline member")
	}
	if !strings.Contains(err.Error(), MemberTimeline) {
		t.Errorf("error %q does not name the member it failed on", err)
	}

	b, err := Start(Config{Dir: t.TempDir(), Interval: -1, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("Start after a failed Start: %v (the failed Start left a CPU profile running)", err)
	}
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
}

// TestStartFailsWhileAnotherCPUProfileIsRunning: the CPU profile is process-global,
// so a second bundle cannot capture one. The second Start reports it rather than
// returning a bundle that would silently carry an empty profile.
func TestStartFailsWhileAnotherCPUProfileIsRunning(t *testing.T) {
	first, err := Start(Config{Dir: t.TempDir(), Interval: -1, Clock: clock.NewManual(epoch)})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	t.Cleanup(func() { _ = first.Stop() })

	second, err := Start(Config{Dir: t.TempDir(), Interval: -1, Clock: clock.NewManual(epoch)})
	if err == nil {
		_ = second.Stop()
		t.Fatal("a second bundle started a CPU profile while one was already running")
	}
	if !strings.Contains(err.Error(), "cpu profile") {
		t.Errorf("error %q does not say the cpu profile could not be started", err)
	}
}

// TestWriteProfileRejectsAProfileTheRuntimeDoesNotKnow. A missing member would be
// read as "this process had no such stacks"; an internal error must not be able to
// masquerade as evidence.
func TestWriteProfileRejectsAProfileTheRuntimeDoesNotKnow(t *testing.T) {
	b := &Bundle{cfg: Config{Dir: t.TempDir()}}

	err := b.writeProfile("bogus.pprof", "no-such-profile", 0)
	if err == nil {
		t.Fatal("writeProfile accepted a profile the runtime does not publish")
	}
	if !strings.Contains(err.Error(), "no-such-profile") {
		t.Errorf("error %q does not name the unknown profile", err)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.Dir, "bogus.pprof")); !os.IsNotExist(err) {
		t.Error("a rejected profile still created its member file")
	}
}

// TestWriteProfileFailsWhenTheMemberCannotBeCreated: a member that cannot be opened
// is reported by name, so Stop's joined error says which evidence is missing.
func TestWriteProfileFailsWhenTheMemberCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	mkdirMember(t, dir, MemberGoroutine)
	b := &Bundle{cfg: Config{Dir: dir}}

	err := b.writeProfile(MemberGoroutine, "goroutine", 0)
	if err == nil {
		t.Fatal("writeProfile succeeded with an unopenable member")
	}
	if !strings.Contains(err.Error(), MemberGoroutine) {
		t.Errorf("error %q does not name the member it failed on", err)
	}
}

// TestWriteManifestFailsWhenTheBundleDirIsGone: the manifest hashes the directory
// it is written into, so a directory that vanished under the capture is reported
// rather than sealed as an empty member list.
func TestWriteManifestFailsWhenTheBundleDirIsGone(t *testing.T) {
	b := &Bundle{
		cfg: Config{Dir: filepath.Join(t.TempDir(), "never-created")},
		clk: clock.NewManual(epoch),
	}

	err := b.writeManifest()
	if err == nil {
		t.Fatal("writeManifest succeeded over a bundle directory that does not exist")
	}
	if !strings.Contains(err.Error(), "read bundle dir") {
		t.Errorf("error %q does not say the bundle directory could not be read", err)
	}
}

// TestWriteManifestFailsWhenTheManifestCannotBeCreated: the members hash fine, and
// only the manifest itself cannot be opened. The failure is still reported, because
// a bundle with no manifest is a bundle whose contents cannot be shown to be intact.
func TestWriteManifestFailsWhenTheManifestCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MemberHeap), []byte("member"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkdirMember(t, dir, MemberManifest)
	b := &Bundle{cfg: Config{Dir: dir}, clk: clock.NewManual(epoch)}

	err := b.writeManifest()
	if err == nil {
		t.Fatal("writeManifest succeeded with an unopenable manifest member")
	}
	if !strings.Contains(err.Error(), MemberManifest) {
		t.Errorf("error %q does not name the manifest", err)
	}
}

// TestHashMemberFailsOnAMemberItCannotOpen. hashMembers walks the directory this
// process wrote, so an unreadable entry means the bundle changed under the capture:
// it is an error, not a member silently omitted from the manifest.
func TestHashMemberFailsOnAMemberItCannotOpen(t *testing.T) {
	_, err := hashMember(t.TempDir(), "not-there")
	if err == nil {
		t.Fatal("hashMember succeeded on a file that does not exist")
	}
	if !strings.Contains(err.Error(), "not-there") {
		t.Errorf("error %q does not name the member it failed on", err)
	}
}

// TestHashMembersSkipsDirectoriesAndTheManifest pins what the manifest hashes: the
// regular files beside it, never a nested directory and never itself.
func TestHashMembersSkipsDirectoriesAndTheManifest(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{MemberHeap, MemberManifest} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mkdirMember(t, dir, "nested")

	members, err := hashMembers(dir, MemberManifest)
	if err != nil {
		t.Fatalf("hashMembers: %v", err)
	}
	if len(members) != 1 || members[0].Name != MemberHeap {
		t.Fatalf("members = %+v, want only %s", members, MemberHeap)
	}
	if members[0].Bytes != int64(len(MemberHeap)) || members[0].SHA256 == "" {
		t.Errorf("member = %+v, want the size and digest of its contents", members[0])
	}
}

// TestWriteAtomicFailsWhenTheTemporaryCannotBeCreated: a dump whose temporary file
// cannot be opened is reported, so the watchdog's log record says the evidence is
// missing instead of naming a file nobody wrote.
func TestWriteAtomicFailsWhenTheTemporaryCannotBeCreated(t *testing.T) {
	b := &Bundle{cfg: Config{Dir: t.TempDir()}}

	err := b.writeAtomic(filepath.Join("no-such-dir", "member.txt"), func(*os.File) error { return nil })
	if err == nil {
		t.Fatal("writeAtomic succeeded under a directory that does not exist")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("error %q does not say the member could not be created", err)
	}
}

// TestWriteAtomicFailsWhenTheMemberCannotBeCommitted: the write succeeds and only
// the rename into place fails. The temporary must not survive it, or the manifest
// would hash a carcass.
func TestWriteAtomicFailsWhenTheMemberCannotBeCommitted(t *testing.T) {
	dir := t.TempDir()
	mkdirMember(t, dir, "member.txt") // a directory cannot be replaced by a rename
	b := &Bundle{cfg: Config{Dir: dir}}

	err := b.writeAtomic("member.txt", func(f *os.File) error {
		_, werr := f.WriteString("evidence")
		return werr
	})
	if err == nil {
		t.Fatal("writeAtomic committed a member over a directory")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Errorf("error %q does not say the member could not be committed", err)
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(partials) != 0 {
		t.Errorf("a failed commit left %v behind", partials)
	}
}

// TestDumpLeakReportsEveryMemberItCouldNotWrite: a dump into a directory that no
// longer exists writes nothing and names no member, so the log record reports the
// failure rather than pointing an operator at files that are not there.
func TestDumpLeakReportsEveryMemberItCouldNotWrite(t *testing.T) {
	b := &Bundle{
		cfg:       Config{Dir: filepath.Join(t.TempDir(), "never-created")},
		leakDumps: map[string]int{},
	}

	names, err := b.dumpLeak(Finding{Counter: "leaky", Window: []Sample{{Goroutines: 3}}})
	if err == nil {
		t.Fatal("dumpLeak succeeded with no bundle directory")
	}
	if len(names) != 0 {
		t.Errorf("dumpLeak named %v, want no members: none were written", names)
	}
	// Every member is attempted, so the joined error accounts for all four rather
	// than abandoning the dump at the first failure.
	for _, member := range []string{"goroutine.labels.txt", "goroutine.txt", MemberHeap, "window.jsonl"} {
		if !strings.Contains(err.Error(), "leak.leaky.0."+member) {
			t.Errorf("the joined error does not report the failed member %q: %v", member, err)
		}
	}
}

// TestReportLogsADumpFailure: the counter and the slope are the two facts an
// operator needs first, and a full disk is the likeliest reason a dump failed, so a
// failed dump is still logged, with the error on the record.
func TestReportLogsADumpFailure(t *testing.T) {
	log := &captureLogger{}
	w, err := newWatchdog(
		LeakConfig{Window: 4, Logger: log, Thresholds: map[string]Threshold{CounterGoroutines: {MinSlope: 1, MinDelta: 4}}},
		func(Finding) ([]string, error) { return nil, errSentinel },
	)
	if err != nil {
		t.Fatalf("newWatchdog: %v", err)
	}

	for i := range 4 {
		w.push(Sample{Goroutines: 10 + i*10})
	}

	if got := log.warnings(); got != 1 {
		t.Fatalf("watchdog logged %d warnings, want 1 even though the dump failed", got)
	}
	rec := log.records[0]
	logged, ok := rec.field("error").(error)
	if !ok || !errors.Is(logged, errSentinel) {
		t.Errorf("log record carries error %v, want the dump failure %v", rec.field("error"), errSentinel)
	}
	if rec.field("counter") != CounterGoroutines {
		t.Errorf("log record names counter %v, want %q", rec.field("counter"), CounterGoroutines)
	}
	if dumps, ok := rec.field("dumps").([]string); ok && len(dumps) != 0 {
		t.Errorf("a failed dump reported files %v", dumps)
	}
}

// TestFindingAtIsTheLastSampleTime: a finding is reported at the moment the growth
// became sustained, which is its last sample, and an empty window has no such moment.
func TestFindingAtIsTheLastSampleTime(t *testing.T) {
	if got := (Finding{}).At(); !got.IsZero() {
		t.Errorf("an empty finding is At %v, want the zero time", got)
	}

	last := epoch.Add(3 * time.Second)
	f := Finding{Window: []Sample{{T: epoch}, {T: epoch.Add(time.Second)}, {T: last}}}
	if got := f.At(); !got.Equal(last) {
		t.Errorf("At() = %v, want the last sample's time %v", got, last)
	}
}

// TestStaircaseNeedsAWindowItCanSplitInThree: a window of fewer than three samples
// has no middle block to separate, so it cannot show sustained growth at all.
func TestStaircaseNeedsAWindowItCanSplitInThree(t *testing.T) {
	for _, vs := range [][]float64{nil, {1}, {1, 2}} {
		if staircase(vs) {
			t.Errorf("staircase(%v) = true, want false: it cannot be split into three blocks", vs)
		}
	}
	if !staircase([]float64{1, 2, 3}) {
		t.Error("staircase([1 2 3]) = false, want true: one sample above one above one")
	}
}

// TestWriteSamplesReportsTheFirstEncodeFailure: the window a leak fired on is
// written through the same JSONL shape as the timeline, and a writer that dies
// mid-dump is reported rather than leaving a truncated window that reads as a
// shorter one.
func TestWriteSamplesReportsTheFirstEncodeFailure(t *testing.T) {
	var buf bytes.Buffer
	w := &failWriter{inner: &buf, failOn: 2}

	samples := []Sample{{Goroutines: 1}, {Goroutines: 2}, {Goroutines: 3}}
	err := writeSamples(w, samples)
	if !errors.Is(err, errSentinel) {
		t.Fatalf("writeSamples error = %v, want the injected failure", err)
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Errorf("writeSamples wrote %d lines after failing on the second, want 1", got)
	}

	// The same samples through a working writer are one JSONL line each.
	buf.Reset()
	if err := writeSamples(&buf, samples); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}
	if got := strings.Count(buf.String(), "\n"); got != len(samples) {
		t.Errorf("writeSamples wrote %d lines, want %d", got, len(samples))
	}
}

// stoppedTimeline builds a timelineWriter with no sampler goroutine behind it, so
// stop's failure handling is exercised on its own: done is already closed, which is
// what stop waits for.
func stoppedTimeline(clk clock.Timing, f *os.File, out io.Writer) *timelineWriter {
	done := make(chan struct{})
	close(done)
	return &timelineWriter{
		clk:      clk,
		interval: time.Second,
		quit:     make(chan struct{}),
		done:     done,
		f:        f,
		enc:      json.NewEncoder(out),
	}
}

// TestTimelineStopReportsAFailedWrite: the first write error is kept and reported by
// stop, so a timeline that lost a line does not close as if it were complete.
func TestTimelineStopReportsAFailedWrite(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), MemberTimeline))
	if err != nil {
		t.Fatal(err)
	}
	w := stoppedTimeline(clock.NewManual(epoch), f, &failWriter{inner: io.Discard})

	err = w.stop()
	if !errors.Is(err, errSentinel) {
		t.Fatalf("stop() = %v, want the injected write failure", err)
	}
	if !strings.Contains(err.Error(), MemberTimeline) {
		t.Errorf("error %q does not name the timeline member", err)
	}
}

// TestTimelineStopReportsAFailedClose: the sample is written and only the member's
// close fails. A bundle whose timeline may not have reached the disk says so.
func TestTimelineStopReportsAFailedClose(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), MemberTimeline))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil { // stop's own Close will now fail
		t.Fatal(err)
	}
	w := stoppedTimeline(clock.NewManual(epoch), f, io.Discard)

	err = w.stop()
	if err == nil {
		t.Fatal("stop() = nil over a member that could not be closed")
	}
	if !strings.Contains(err.Error(), "close "+MemberTimeline) {
		t.Errorf("error %q does not say the timeline member could not be closed", err)
	}
}

// TestStopReleasesASamplerParkedOnASample: the sampler offers every sample to the
// test channel and blocks there. Stop must release it rather than deadlock against
// a reader that has gone away, which is the same shape as a watchdog dump that
// outlives the process's exit path.
func TestStopReleasesASamplerParkedOnASample(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewManual(epoch)
	sampled := make(chan struct{}) // never read from: the sampler parks on the send

	b, err := Start(Config{Dir: dir, Interval: time.Second, Clock: clk, sampled: sampled})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Advance one tick and wait until the sample is on disk, which is the sampler's
	// last step before it parks on the send nobody is reading.
	timeline := filepath.Join(dir, MemberTimeline)
	baseline := fileSize(t, timeline)
	clk.Advance(time.Second)
	waitForGrowth(t, timeline, baseline)

	if err := b.Stop(); err != nil {
		t.Fatalf("Stop while the sampler was parked: %v", err)
	}

	// The baseline, the tick that parked, and Stop's final sample: nothing was lost
	// by releasing the parked sampler.
	if got := len(readTimeline(t, dir)); got != 3 {
		t.Errorf("timeline has %d samples, want 3 (baseline, one tick, final)", got)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// waitForGrowth blocks until path is larger than was, which is how a test observes
// the sampler having written a line without racing its next step. The bounded loop
// is a test-failure guard, not part of the behaviour under test.
func waitForGrowth(t *testing.T, path string, was int64) {
	t.Helper()
	for range 2000 {
		if fi, err := os.Stat(path); err == nil && fi.Size() > was {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not grow past %d bytes: the sampler never wrote its tick", path, was)
}
