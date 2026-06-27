package serve

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/sandbox"
)

// fakeServing is a scripted container-backed Serving: it reports a fixed container identity
// and lets a test drive its exit, so the container lifecycle is exercised without a real
// engine.
type fakeServing struct {
	id, engine string
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	stopped    bool
}

func newFakeServing(id, engine string) *fakeServing {
	return &fakeServing{id: id, engine: engine, done: make(chan struct{})}
}

func (f *fakeServing) Addr() string { return "127.0.0.1:9000" }
func (f *fakeServing) Running() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}
func (f *fakeServing) Output() string        { return "container log tail" }
func (f *fakeServing) Done() <-chan struct{} { return f.done }
func (f *fakeServing) Stop() error {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.exit()
	return nil
}
func (f *fakeServing) ContainerID() string { return f.id }
func (f *fakeServing) EngineName() string  { return f.engine }
func (f *fakeServing) exit()               { f.once.Do(func() { close(f.done) }) }
func (f *fakeServing) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// containerRunner returns a runner that always yields s, recording that it was called.
func containerRunner(s sandbox.Serving, called *bool) func(context.Context, sandbox.ContainerSpec) (sandbox.Serving, error) {
	return func(context.Context, sandbox.ContainerSpec) (sandbox.Serving, error) {
		if called != nil {
			*called = true
		}
		return s, nil
	}
}

func containerManager(t *testing.T, run func(context.Context, sandbox.ContainerSpec) (sandbox.Serving, error), probe Prober, stop ContainerStopper) (*Manager, *Registry) {
	t.Helper()
	reg := NewRegistry(t.TempDir())
	m := NewManager(
		&fakeLauncher{proc: newFakeProc(1)}, probe, OSKiller, reg,
		WithReadyTimeout(2*time.Second),
		WithPollInterval(2*time.Millisecond),
		WithContainerStopper(stop),
		withContainerRunner(run),
		withClock(clock.NewManual(time.Unix(1000, 0))),
	)
	return m, reg
}

func containerEnsureCfg() ContainerEnsureConfig {
	return ContainerEnsureConfig{
		ModelID: "vllm:qwen2.5-coder:7b-awq",
		Runtime: "vllm",
		Spec:    sandbox.ContainerSpec{}, // ignored by the injected runner
		BaseURL: "http://127.0.0.1:9000/v1",
		Port:    9000,
	}
}

func TestEnsureContainerStartsAndRecords(t *testing.T) {
	s := newFakeServing("c0ffee123456", "docker")
	m, reg := containerManager(t, containerRunner(s, nil), healthyAfter(1), nil)

	ep, err := m.EnsureContainer(context.Background(), containerEnsureCfg())
	if err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	if ep.Reused || ep.Port != 9000 || ep.BaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("endpoint = %+v, want a fresh container endpoint on 9000", ep)
	}
	// The record carries the container identity, not a pid, so a later process can stop it.
	rec, ok, err := reg.Get("vllm:qwen2.5-coder:7b-awq")
	if err != nil || !ok {
		t.Fatalf("record not persisted: ok=%v err=%v", ok, err)
	}
	if rec.ContainerID != "c0ffee123456" || rec.Engine != "docker" || rec.PID != 0 {
		t.Fatalf("record should carry the container identity, got %+v", rec)
	}
	// The endpoint controls the started container.
	if err := ep.Stop(); err != nil || !s.wasStopped() {
		t.Fatalf("endpoint stop should stop the container: err=%v stopped=%v", err, s.wasStopped())
	}
}

func TestEnsureContainerReusesHealthy(t *testing.T) {
	called := false
	s := newFakeServing("id", "docker")
	m, reg := containerManager(t, containerRunner(s, &called), alwaysHealthy, nil)
	// Pre-record a running container; a healthy probe should adopt it without starting one.
	_ = reg.Put(Record{ModelID: "vllm:m", BaseURL: "http://127.0.0.1:9000/v1", Port: 9000, ContainerID: "existing", Engine: "docker"})

	cfg := containerEnsureCfg()
	cfg.ModelID = "vllm:m"
	ep, err := m.EnsureContainer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Reused {
		t.Fatal("a healthy recorded container must be reused")
	}
	if called {
		t.Fatal("reuse must not start a new container")
	}
}

func TestEnsureContainerExitsWhileStarting(t *testing.T) {
	s := newFakeServing("id", "docker")
	s.exit() // the container is already gone before it ever becomes ready
	m, _ := containerManager(t, containerRunner(s, nil), neverHealthy, nil)

	if _, err := m.EnsureContainer(context.Background(), containerEnsureCfg()); err == nil {
		t.Fatal("a container that exits while starting must be an error, not a silent success")
	}
}

func TestStopRoutesContainerToEngineNotKill(t *testing.T) {
	var stopped [2]string
	stopper := func(engine, id string) error { stopped = [2]string{engine, id}; return nil }
	reg := NewRegistry(t.TempDir())
	killed := -1
	m := NewManager(&fakeLauncher{proc: newFakeProc(1)}, alwaysHealthy,
		func(pid int) error { killed = pid; return nil }, reg, WithContainerStopper(stopper))

	_ = reg.Put(Record{ModelID: "vllm:m", BaseURL: "u", Port: 9000, ContainerID: "cid", Engine: "podman"})
	ok, err := m.Stop("vllm:m")
	if err != nil || !ok {
		t.Fatalf("stop: ok=%v err=%v", ok, err)
	}
	if stopped != [2]string{"podman", "cid"} {
		t.Fatalf("a container record must be stopped via the engine, got %v", stopped)
	}
	if killed != -1 {
		t.Fatalf("a container record must not be killed by pid, killed=%d", killed)
	}

	// A process-backed record still routes to the killer, not the engine stopper.
	stopped = [2]string{}
	_ = reg.Put(Record{ModelID: "llama:m", BaseURL: "u", Port: 8080, PID: 4321})
	if _, err := m.Stop("llama:m"); err != nil {
		t.Fatal(err)
	}
	if killed != 4321 || stopped != [2]string{} {
		t.Fatalf("a process record must be killed by pid, killed=%d stopped=%v", killed, stopped)
	}
}

func TestStatusReclaimsDeadContainerViaEngine(t *testing.T) {
	var stoppedID string
	stopper := func(_, id string) error { stoppedID = id; return nil }
	reg := NewRegistry(t.TempDir())
	m := NewManager(&fakeLauncher{proc: newFakeProc(1)}, neverHealthy, OSKiller, reg, WithContainerStopper(stopper))
	_ = reg.Put(Record{ModelID: "vllm:m", BaseURL: "u", Port: 9000, ContainerID: "deadcid", Engine: "docker"})

	live, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("a dead container must be pruned, got %d live", len(live))
	}
	if stoppedID != "deadcid" {
		t.Fatalf("a reclaimed container must be stopped via the engine, got %q", stoppedID)
	}
}
