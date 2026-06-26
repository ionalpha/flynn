package serve

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inference/launch"
	"github.com/ionalpha/flynn/sandbox"
)

// fakeProc is a scripted stand-in for a started server process. It never runs anything;
// a test drives its lifecycle directly.
type fakeProc struct {
	pid     int
	output  string
	done    chan struct{}
	mu      sync.Mutex
	stopped bool // Stop was called
	closed  bool // done has been closed (by Stop or by exiting on its own)
}

func newFakeProc(pid int) *fakeProc { return &fakeProc{pid: pid, done: make(chan struct{})} }

func (f *fakeProc) PID() int { return f.pid }

func (f *fakeProc) Running() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}

func (f *fakeProc) Output() string { return f.output }

func (f *fakeProc) Done() <-chan struct{} { return f.done }

func (f *fakeProc) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

func (f *fakeProc) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// exitNow marks the process as exited on its own (not via Stop), the way a runtime that
// fails to come up behaves.
func (f *fakeProc) exitNow(output string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.output = output
		f.closed = true
		close(f.done)
	}
}

// fakeLauncher hands out a pre-built fakeProc and records the spec it was asked to run.
type fakeLauncher struct {
	proc    *fakeProc
	err     error
	gotSpec sandbox.ServeSpec
	calls   int
}

func (l *fakeLauncher) Serve(_ context.Context, spec sandbox.ServeSpec) (Proc, error) {
	l.calls++
	l.gotSpec = spec
	if l.err != nil {
		return nil, l.err
	}
	return l.proc, nil
}

// healthyAfter returns a prober that fails for the first n probes, then succeeds, so a
// test can model a server that takes a few polls to come up.
func healthyAfter(n int) Prober {
	var count int
	var mu sync.Mutex
	return func(context.Context, string) error {
		mu.Lock()
		defer mu.Unlock()
		count++
		if count <= n {
			return errors.New("not ready")
		}
		return nil
	}
}

// alwaysHealthy and neverHealthy are the two constant probers.
func alwaysHealthy(context.Context, string) error { return nil }
func neverHealthy(context.Context, string) error  { return errors.New("down") }

func testManager(t *testing.T, l Launcher, probe Prober, kill Killer) (*Manager, *Registry) {
	t.Helper()
	reg := NewRegistry(t.TempDir())
	m := NewManager(
		l, probe, kill, reg,
		WithReadyTimeout(2*time.Second),
		WithPollInterval(2*time.Millisecond),
		withClock(clock.NewManual(time.Unix(1000, 0))),
	)
	return m, reg
}

func samplePlan(port int) launch.Plan {
	return launch.Plan{
		Argv:    []string{"/runtimes/llama-server", "--model", "/models/x.gguf", "--port"},
		Host:    "127.0.0.1",
		Port:    port,
		BaseURL: "http://127.0.0.1:" + itoa(port) + "/v1",
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestEnsureStartsAndRecords(t *testing.T) {
	proc := newFakeProc(4242)
	l := &fakeLauncher{proc: proc}
	m, reg := testManager(t, l, healthyAfter(2), OSKiller)

	ep, err := m.Ensure(context.Background(), EnsureConfig{
		ModelID: "ollama:qwen2.5-coder:1.5b", Runtime: "llama.cpp", Plan: samplePlan(8080), Confine: true,
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if ep.Reused {
		t.Fatal("a freshly started server must not be reported as reused")
	}
	if ep.PID != 4242 || ep.Port != 8080 {
		t.Fatalf("endpoint = %+v, want pid 4242 port 8080", ep)
	}
	if !l.gotSpec.Confine {
		t.Fatal("Confine must be passed through to the launcher")
	}
	rec, ok, _ := reg.Get("ollama:qwen2.5-coder:1.5b")
	if !ok || rec.PID != 4242 || rec.StartedAt != 1000 {
		t.Fatalf("registry record = %+v ok=%v, want pid 4242 startedAt 1000", rec, ok)
	}
}

func TestEnsureReusesHealthyServer(t *testing.T) {
	l := &fakeLauncher{proc: newFakeProc(1)}
	m, reg := testManager(t, l, alwaysHealthy, OSKiller)
	if err := reg.Put(Record{ModelID: "m", PID: 99, Port: 9000, BaseURL: "http://127.0.0.1:9000/v1"}); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Ensure(context.Background(), EnsureConfig{ModelID: "m", Plan: samplePlan(9000)})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !ep.Reused || ep.PID != 99 {
		t.Fatalf("expected to reuse pid 99, got %+v", ep)
	}
	if l.calls != 0 {
		t.Fatal("a reused server must not launch a new process")
	}
}

func TestEnsurePrunesStaleRecordThenStarts(t *testing.T) {
	proc := newFakeProc(7)
	l := &fakeLauncher{proc: proc}
	// The recorded server is dead (never healthy); the freshly started one is healthy.
	probes := 0
	probe := func(context.Context, string) error {
		probes++
		if probes == 1 {
			return errors.New("stale endpoint down") // the reuse check
		}
		return nil // the started server answers
	}
	m, reg := testManager(t, l, probe, OSKiller)
	if err := reg.Put(Record{ModelID: "m", PID: 5, Port: 9000, BaseURL: "http://127.0.0.1:9000/v1"}); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Ensure(context.Background(), EnsureConfig{ModelID: "m", Plan: samplePlan(9001)})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if ep.Reused || ep.PID != 7 {
		t.Fatalf("expected a fresh start with pid 7, got %+v", ep)
	}
	if l.calls != 1 {
		t.Fatalf("expected exactly one launch, got %d", l.calls)
	}
}

func TestEnsureProcessExitsWhileStarting(t *testing.T) {
	proc := newFakeProc(8)
	l := &fakeLauncher{proc: proc}
	m, reg := testManager(t, l, neverHealthy, OSKiller)
	// The runtime dies right after launch, before its endpoint ever answers.
	go func() {
		time.Sleep(5 * time.Millisecond)
		proc.exitNow("error: failed to load model: bad magic")
	}()
	_, err := m.Ensure(context.Background(), EnsureConfig{ModelID: "m", Plan: samplePlan(9000)})
	if err == nil {
		t.Fatal("expected an error when the runtime exits while starting")
	}
	if got := err.Error(); !contains(got, "bad magic") {
		t.Fatalf("error should carry the runtime output, got %q", got)
	}
	if _, ok, _ := reg.Get("m"); ok {
		t.Fatal("a failed start must not leave a registry record")
	}
}

func TestEnsureTimesOut(t *testing.T) {
	proc := newFakeProc(9)
	l := &fakeLauncher{proc: proc}
	reg := NewRegistry(t.TempDir())
	m := NewManager(l, neverHealthy, OSKiller, reg,
		WithReadyTimeout(20*time.Millisecond), WithPollInterval(2*time.Millisecond))
	_, err := m.Ensure(context.Background(), EnsureConfig{ModelID: "m", Plan: samplePlan(9000)})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !proc.wasStopped() {
		t.Fatal("a server that never becomes ready must be stopped")
	}
	if _, ok, _ := reg.Get("m"); ok {
		t.Fatal("a timed-out start must not leave a registry record")
	}
}

func TestEnsureRejectsBadConfig(t *testing.T) {
	m, _ := testManager(t, &fakeLauncher{proc: newFakeProc(1)}, alwaysHealthy, OSKiller)
	if _, err := m.Ensure(context.Background(), EnsureConfig{ModelID: "", Plan: samplePlan(1)}); err == nil {
		t.Fatal("expected an error for an empty model id")
	}
	if _, err := m.Ensure(context.Background(), EnsureConfig{ModelID: "m", Plan: launch.Plan{}}); err == nil {
		t.Fatal("expected an error for an empty plan")
	}
}

func TestStatusPrunesDeadReturnsLive(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	_ = reg.Put(Record{ModelID: "live", BaseURL: "http://127.0.0.1:1/v1"})
	_ = reg.Put(Record{ModelID: "dead", BaseURL: "http://127.0.0.1:2/v1"})
	probe := func(_ context.Context, base string) error {
		if contains(base, ":1/") {
			return nil
		}
		return errors.New("down")
	}
	m := NewManager(&fakeLauncher{proc: newFakeProc(1)}, probe, OSKiller, reg)
	live, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(live) != 1 || live[0].ModelID != "live" {
		t.Fatalf("expected only the live server, got %+v", live)
	}
	if _, ok, _ := reg.Get("dead"); ok {
		t.Fatal("Status must prune a dead record")
	}
}

func TestStopKillsByPIDAndRemoves(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	_ = reg.Put(Record{ModelID: "m", PID: 12345, BaseURL: "http://127.0.0.1:1/v1"})
	var killed int
	kill := func(pid int) error { killed = pid; return nil }
	m := NewManager(&fakeLauncher{proc: newFakeProc(1)}, alwaysHealthy, kill, reg)

	found, err := m.Stop("m")
	if err != nil || !found {
		t.Fatalf("Stop = (%v, %v), want (true, nil)", found, err)
	}
	if killed != 12345 {
		t.Fatalf("expected to kill pid 12345, killed %d", killed)
	}
	if _, ok, _ := reg.Get("m"); ok {
		t.Fatal("Stop must remove the record")
	}
	// Stopping an unknown model reports not-found, not an error.
	found, err = m.Stop("absent")
	if err != nil || found {
		t.Fatalf("Stop(absent) = (%v, %v), want (false, nil)", found, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
