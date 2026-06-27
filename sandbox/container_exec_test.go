package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCID is a well-formed 64-character container id the scripted engine returns.
const fakeCID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// fakeEngine is a scripted OCI engine: it records every command and answers by subcommand,
// so the driver's run/adopt/lifecycle logic is exercised without a real engine. A `wait`
// blocks until the container is released (or ctx is done), modeling a container that runs
// until it exits or is stopped.
type fakeEngine struct {
	mu         sync.Mutex
	calls      [][]string
	runID      string
	runErr     error
	versionOut string
	versionErr error
	gate       chan struct{}
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{runID: fakeCID, versionOut: "29.2.1", gate: make(chan struct{})}
}

func (f *fakeEngine) runner(ctx context.Context, argv []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	f.mu.Unlock()
	sub := ""
	if len(argv) > 1 {
		sub = argv[1]
	}
	switch sub {
	case "version":
		return f.versionOut, f.versionErr
	case "run":
		return f.runID, f.runErr
	case "wait":
		select {
		case <-f.gate:
			return "0\n", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	case "logs":
		return "a log line\n", nil
	case "stop":
		f.release() // stopping the container releases the wait
		return "", nil
	}
	return "", nil
}

// release simulates the container exiting, unblocking the reaper's wait. It is safe to call
// more than once.
func (f *fakeEngine) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.gate:
	default:
		close(f.gate)
	}
}

func (f *fakeEngine) sawSubcommand(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == sub {
			return true
		}
	}
	return false
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the serving to report done within the timeout")
	}
}

func TestOCIDriverDetect(t *testing.T) {
	ok := &fakeEngine{versionOut: "29.2.1", gate: make(chan struct{})}
	if av := NewContainerDriver(EngineDocker, ok.runner).Detect(); !av.OK || !strings.Contains(av.Detail, "29.2.1") {
		t.Fatalf("a usable engine should report available: %+v", av)
	}
	down := &fakeEngine{versionErr: errors.New("cannot connect to the daemon"), gate: make(chan struct{})}
	if av := NewContainerDriver(EngineDocker, down.runner).Detect(); av.OK {
		t.Fatalf("an engine whose daemon is down must report unavailable: %+v", av)
	}
	empty := &fakeEngine{versionOut: "  \n", gate: make(chan struct{})}
	if av := NewContainerDriver(EnginePodman, empty.runner).Detect(); av.OK {
		t.Fatalf("an engine that reports no server version must be unavailable: %+v", av)
	}
}

func TestOCIDriverRunAdoptsAndReaps(t *testing.T) {
	f := newFakeEngine()
	d := NewContainerDriver(EngineDocker, f.runner)
	s, err := d.Run(context.Background(), servingSpec())
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr() != "127.0.0.1:8123" {
		t.Fatalf("a publishing container should answer on its loopback host port, got %q", s.Addr())
	}
	if !s.Running() {
		t.Fatal("a freshly started container should be running")
	}
	// The recorded run command is the hardened, detached argv the tier composes.
	f.mu.Lock()
	first := f.calls[0]
	f.mu.Unlock()
	if first[1] != "run" || !argvHas(first, "--detach") || !argvHas(first, "--read-only") {
		t.Fatalf("the first call should be the hardened detached run: %v", first)
	}
	// Releasing the container closes done and flips Running.
	f.release()
	waitClosed(t, s.Done())
	if s.Running() {
		t.Fatal("an exited container must not report running")
	}
}

func TestOCIDriverRunRejectsBadID(t *testing.T) {
	for name, id := range map[string]string{"empty": "", "short": "abc", "non-hex": strings.Repeat("z", 64)} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEngine()
			f.runID = id
			if _, err := NewContainerDriver(EngineDocker, f.runner).Run(context.Background(), servingSpec()); err == nil {
				t.Fatalf("a run that returns %q must not be adopted as a container", id)
			}
		})
	}
}

func TestOCIDriverRunPropagatesEngineError(t *testing.T) {
	f := newFakeEngine()
	f.runErr = errors.New("no such image")
	if _, err := NewContainerDriver(EngineDocker, f.runner).Run(context.Background(), servingSpec()); err == nil {
		t.Fatal("a failed run must surface an error")
	}
}

func TestContainerServingStopIsIdempotent(t *testing.T) {
	f := newFakeEngine()
	s, err := NewContainerDriver(EngineDocker, f.runner).Run(context.Background(), servingSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitClosed(t, s.Done())
	if !f.sawSubcommand("stop") {
		t.Fatal("stop should have asked the engine to stop the container")
	}
	// A second stop on an exited container is a no-op.
	if err := s.Stop(); err != nil {
		t.Fatalf("second stop must be a no-op: %v", err)
	}
}

func TestContainerServingNonPublishingHasNoAddr(t *testing.T) {
	f := newFakeEngine()
	spec := ContainerSpec{Image: ContainerImage{Digest: validDigest}, Guarantees: Untrusted(Limits{MemMiB: 512})}
	s, err := NewContainerDriver(EnginePodman, f.runner).Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr() != "" {
		t.Fatalf("a non-publishing container has no address, got %q", s.Addr())
	}
	f.release()
	waitClosed(t, s.Done())
}

func TestPlausibleContainerID(t *testing.T) {
	cases := map[string]bool{
		fakeCID:                 true,
		"abcdef012345":          true, // 12-char short id
		"abcdef01234":           false,
		"":                      false,
		strings.Repeat("z", 64): false,
		"abc def":               false,
	}
	for s, want := range cases {
		if got := plausibleContainerID(s); got != want {
			t.Fatalf("plausibleContainerID(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestLastLineAndOneLine(t *testing.T) {
	if got := lastLine("pulling image\n" + fakeCID + "\n"); got != fakeCID {
		t.Fatalf("lastLine should return the id after a progress line, got %q", got)
	}
	if got := lastLine("  \n\n"); got != "" {
		t.Fatalf("lastLine of blank input should be empty, got %q", got)
	}
	if got := oneLine("error:\n  cannot   connect\n"); got != "error: cannot connect" {
		t.Fatalf("oneLine should collapse whitespace, got %q", got)
	}
}
