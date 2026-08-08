package e2e

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bundle's layout is a contract with whatever reads it later, so this suite
// parses the manifest against its own copy of the schema rather than importing the
// producing package. A field renamed on one side and not the other fails here.
type bundleManifest struct {
	BundleID     string            `json:"bundle_id"`
	FlynnVersion string            `json:"flynn_version"`
	GoVersion    string            `json:"go_version"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Args         []string          `json:"args"`
	Contention   bool              `json:"contention"`
	Annotations  map[string]string `json:"annotations"`
	Members      []struct {
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"members"`
}

type bundleSample struct {
	T              string `json:"t"`
	Goroutines     int    `json:"goroutines"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapLiveBytes  int64  `json:"heap_live_bytes"`
	OpenFDs        int    `json:"open_fds"`
	ChildProcs     int    `json:"child_procs"`
}

// profileMembers are the members every bundle carries, contention aside.
var profileMembers = []string{
	"cpu.pprof", "heap.pprof", "allocs.pprof", "goroutine.pprof",
	"goroutine.txt", "threadcreate.pprof", "runtime.jsonl", "manifest.json",
}

// pprofMembers are the members `go tool pprof` must be able to open.
var pprofMembers = []string{"cpu.pprof", "heap.pprof", "allocs.pprof", "goroutine.pprof", "threadcreate.pprof"}

// TestProfileBundleFromAGoalRun drives a real goal against the scripted model with
// --profile and asserts the bundle a support engineer would actually receive: every
// member present, every pprof member openable by the standard tooling, every
// manifest hash matching the bytes on disk, a timeline with a baseline and a final
// sample, and no trace of the objective in the recorded command line.
func TestProfileBundleFromAGoalRun(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("The answer is 42."))
	in := newInstance(t).withModel(fake)
	dir := filepath.Join(t.TempDir(), "bundle") // must be created by the run, not by the test

	res := in.run("-no-learn", "-profile", dir, "goal", "state the answer")
	requireExit(t, res, 0, "goal --profile")
	requireContains(t, res.stdout, "The answer is 42.", "goal output")

	for _, name := range profileMembers {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("bundle member %s missing: %v", name, err)
		}
	}
	// Contention profiling was not asked for, so it did not run.
	for _, name := range []string{"block.pprof", "mutex.pprof"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("bundle member %s written without --profile-contention", name)
		}
	}

	requirePprofParses(t, dir, pprofMembers)
	m := requireManifestIntact(t, dir)

	if m.BundleID == "" {
		t.Error("manifest carries no bundle id")
	}
	if m.OS == "" || m.Arch == "" || m.GoVersion == "" {
		t.Errorf("manifest is missing platform provenance: %+v", m)
	}
	if m.Contention {
		t.Error("manifest reports contention profiling, which was not requested")
	}
	// The objective is free user text: it reaches the command line and must not reach
	// the bundle an operator mails to someone else.
	if args := strings.Join(m.Args, " "); strings.Contains(args, "state the answer") {
		t.Errorf("manifest args leak the objective: %q", args)
	}
	if args := strings.Join(m.Args, " "); !strings.Contains(args, "goal") {
		t.Errorf("manifest args lost the subcommand, leaving nothing to read: %q", args)
	}

	samples := readBundleTimeline(t, dir)
	if len(samples) < 2 {
		t.Fatalf("timeline has %d samples, want at least a baseline and a final", len(samples))
	}
	for i, s := range samples {
		if s.Goroutines < 1 {
			t.Errorf("sample %d reports %d goroutines", i, s.Goroutines)
		}
		if s.HeapAllocBytes == 0 {
			t.Errorf("sample %d reports a zero live heap", i)
		}
		// -1 is the documented "this platform cannot report it"; 0 fds never is.
		if s.OpenFDs == 0 {
			t.Errorf("sample %d reports 0 open fds; a live process holds at least one, and an unmeasurable count is -1", i)
		}
		// -1 here means the binary opened a bundle without telling it how to read the
		// process registry. That is the only way a built binary can report a child count:
		// nothing in the bundle walks the machine's process table to find one.
		if s.ChildProcs < 0 {
			t.Errorf("sample %d reports child_procs %d; the binary must wire its child-process counter into the bundle", i, s.ChildProcs)
		}
	}
}

// TestProfileContentionViaEnvironment asserts the opt-in block and mutex members are
// added when contention profiling is asked for, and that asking through the
// environment works for a process whose command line an operator cannot change.
func TestProfileContentionViaEnvironment(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("done"))
	in := newInstance(t).withModel(fake)
	dir := filepath.Join(t.TempDir(), "bundle")
	in.setEnv("FLYNN_PROFILE", dir)
	in.setEnv("FLYNN_PROFILE_CONTENTION", "true")

	res := in.run("-no-learn", "goal", "do nothing")
	requireExit(t, res, 0, "goal with FLYNN_PROFILE")

	for _, name := range append([]string{"block.pprof", "mutex.pprof"}, profileMembers...) {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("bundle member %s missing: %v", name, err)
		}
	}
	requirePprofParses(t, dir, append([]string{"block.pprof", "mutex.pprof"}, pprofMembers...))

	if m := requireManifestIntact(t, dir); !m.Contention {
		t.Error("manifest does not record that contention profiling was on")
	}
}

// TestProfileBundleSealedOnCommandFailure asserts the bundle survives the failure it
// was most likely captured to explain. A command that exits non-zero must still seal
// its bundle, and must not have its own exit code changed by the profiler.
func TestProfileBundleSealedOnCommandFailure(t *testing.T) {
	in := newInstance(t)
	dir := filepath.Join(t.TempDir(), "bundle")

	res := in.run("-profile", dir, "get", "not-a-kind")
	requireExit(t, res, 1, "get with an unknown kind")

	requireManifestIntact(t, dir)
	requirePprofParses(t, dir, pprofMembers)
}

// TestNoProfileWritesNothing asserts the default path is inert: with no flag and no
// environment variable, the command touches no bundle directory at all.
func TestNoProfileWritesNothing(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("done"))
	in := newInstance(t).withModel(fake)
	dir := filepath.Join(t.TempDir(), "bundle")

	res := in.run("-no-learn", "goal", "do nothing")
	requireExit(t, res, 0, "goal")

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a run with no --profile created %s (err=%v)", dir, err)
	}
}

// TestLeakWatchStaysQuietThroughAnHonestRun is the false-positive gate, run against
// the real binary rather than a synthetic series. A goal loop allocates, spawns, and
// opens descriptors; none of that is a leak, and a watchdog that says otherwise is a
// watchdog an operator turns off. The run must finish with its own exit code, its
// own output, no dump in the bundle, and no warning on stderr.
func TestLeakWatchStaysQuietThroughAnHonestRun(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("The answer is 42."))
	in := newInstance(t).withModel(fake)
	dir := filepath.Join(t.TempDir(), "bundle")

	res := in.run("-no-learn", "-profile", dir, "-leak-watch", "goal", "state the answer")
	requireExit(t, res, 0, "goal --profile --leak-watch")
	requireContains(t, res.stdout, "The answer is 42.", "goal output")

	if strings.Contains(res.stderr, "possible leak") {
		t.Errorf("the watchdog fired on an honest run:\n%s", res.stderr)
	}
	dumps, err := filepath.Glob(filepath.Join(dir, "leak.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dumps) != 0 {
		t.Errorf("the watchdog dumped %v on an honest run", dumps)
	}

	// The bundle is otherwise exactly the bundle --profile alone produces: watching
	// a timeline does not change what is recorded in it.
	requireManifestIntact(t, dir)
	requirePprofParses(t, dir, pprofMembers)

	// The heap counter the detector fits is the live heap, not the allocated heap.
	// Zero is a legitimate reading only before the first collection, so at least one
	// sample must carry a real one for the counter to be worth watching.
	samples := readBundleTimeline(t, dir)
	live := false
	for _, s := range samples {
		if s.HeapLiveBytes > 0 {
			live = true
		}
		if s.HeapLiveBytes < 0 && s.HeapLiveBytes != -1 {
			t.Errorf("sample carries heap_live_bytes = %d, which is neither a size nor the unknown marker", s.HeapLiveBytes)
		}
	}
	if !live && len(samples) > 1 {
		t.Error("no sample carries a live heap; the heap detector would be watching a flat zero")
	}
}

// TestLeakWatchWithoutABundleIsAUsageError. The watchdog samples the bundle's
// timeline and dumps into the bundle. Without one it would watch nothing, silently,
// which is the one failure an operator leaving a week-long soak would never notice.
func TestLeakWatchWithoutABundleIsAUsageError(t *testing.T) {
	in := newInstance(t)

	res := in.run("-leak-watch", "runs")
	requireExit(t, res, 2, "--leak-watch with no --profile")
	requireContains(t, res.stderr, "--profile", "the error names the flag that is missing")
}

// --- helpers -----------------------------------------------------------------

// requireManifestIntact parses the manifest and checks every member's recorded size
// and digest against the bytes on disk. This is the property that lets a bundle be
// copied off a machine and still be reasoned from.
func requireManifestIntact(t *testing.T, dir string) bundleManifest {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m bundleManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Members) == 0 {
		t.Fatal("manifest lists no members")
	}

	for _, mem := range m.Members {
		if mem.Name == "manifest.json" {
			t.Error("manifest lists itself as a member")
		}
		data, err := os.ReadFile(filepath.Join(dir, mem.Name))
		if err != nil {
			t.Errorf("manifest names member %s, which is not in the bundle: %v", mem.Name, err)
			continue
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != mem.SHA256 {
			t.Errorf("member %s: manifest hash %s, actual %s", mem.Name, mem.SHA256, got)
		}
		if int64(len(data)) != mem.Bytes {
			t.Errorf("member %s: manifest size %d, actual %d", mem.Name, mem.Bytes, len(data))
		}
	}

	// Every non-manifest file in the bundle is accounted for, so nothing is shipped
	// unhashed alongside the evidence.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read bundle dir: %v", err)
	}
	if got, want := len(entries)-1, len(m.Members); got != want {
		t.Errorf("bundle holds %d members but the manifest hashes %d", got, want)
	}
	return m
}

// requirePprofParses opens each member with the same tooling an engineer would reach
// for. A profile the standard tool cannot read is not evidence, whatever its bytes say.
func requirePprofParses(t *testing.T, dir string, members []string) {
	t.Helper()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go tool pprof unavailable: %v", err)
	}
	for _, name := range members {
		path := filepath.Join(dir, name)
		out, err := exec.Command(goBin, "tool", "pprof", "-raw", "-output", os.DevNull, path).CombinedOutput()
		if err != nil {
			t.Errorf("go tool pprof cannot open %s: %v\n%s", name, err, out)
		}
	}
}

func readBundleTimeline(t *testing.T, dir string) []bundleSample {
	t.Helper()

	f, err := os.Open(filepath.Join(dir, "runtime.jsonl"))
	if err != nil {
		t.Fatalf("open timeline: %v", err)
	}
	defer func() { _ = f.Close() }()

	var samples []bundleSample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s bundleSample
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
