package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/sandbox"
)

// brokenRegistry returns a registry whose file cannot be read: a directory sits where the
// records belong. Every path that touches the registry must report that rather than acting
// as though no server is recorded, which would start a second one on top of a live model.
func brokenRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "servers.json"), 0o750); err != nil {
		t.Fatalf("make the registry path unreadable: %v", err)
	}
	return NewRegistry(dir)
}

func TestRegistryReportsAnUnreadableFile(t *testing.T) {
	reg := brokenRegistry(t)

	if _, err := reg.List(); err == nil {
		t.Error("List over an unreadable registry must fail")
	}
	if _, _, err := reg.Get("m"); err == nil {
		t.Error("Get over an unreadable registry must fail")
	}
	if err := reg.Put(Record{ModelID: "m"}); err == nil {
		t.Error("Put over an unreadable registry must fail")
	}
	if err := reg.Delete("m"); err == nil {
		t.Error("Delete over an unreadable registry must fail")
	}
}

// TestRegistryReportsAnUnwritableDirectory checks a registry that cannot be persisted says
// so, rather than reporting a server as recorded when nothing was written.
func TestRegistryReportsAnUnwritableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	reg := NewRegistry(filepath.Join(file, "sub"))

	if err := reg.Put(Record{ModelID: "m"}); err == nil {
		t.Fatal("Put must fail when the registry directory cannot be made")
	}
}

// TestACorruptRegistryReadsAsEmpty checks a malformed registry file does not wedge every
// command: it reads as empty so a fresh server can be started and the file rewritten clean.
func TestACorruptRegistryReadsAsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	reg := NewRegistry(dir)

	recs, err := reg.List()
	if err != nil {
		t.Fatalf("List over a corrupt registry: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("List = %v, want an empty registry", recs)
	}
	if err := reg.Put(Record{ModelID: "m", BaseURL: "http://127.0.0.1:1/v1"}); err != nil {
		t.Fatalf("Put over a corrupt registry: %v", err)
	}
	if rec, ok, err := reg.Get("m"); err != nil || !ok || rec.ModelID != "m" {
		t.Errorf("Get = %+v, %v, %v, want the freshly written record", rec, ok, err)
	}
}

// TestAnEmptyRegistryFileIsNotAnError checks a zero-length file (a truncated write, an
// interrupted first run) reads as no records.
func TestAnEmptyRegistryFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), nil, 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	recs, err := NewRegistry(dir).List()
	if err != nil || len(recs) != 0 {
		t.Errorf("List = %v, %v, want no records and no error", recs, err)
	}
}

// managerWith builds a manager over the given registry, with fakes for everything that would
// otherwise touch the host.
func managerWith(t *testing.T, reg *Registry, l Launcher, probe Prober, kill Killer, opts ...Option) *Manager {
	t.Helper()
	base := []Option{
		WithReadyTimeout(2 * time.Second),
		WithPollInterval(2 * time.Millisecond),
		withClock(clock.NewManual(time.Unix(1000, 0))),
	}
	return NewManager(l, probe, kill, reg, append(base, opts...)...)
}

// TestEveryManagerPathReportsABrokenRegistry checks the manager never silently works around a
// registry it cannot read. Starting a second server for a model that is already up would hold
// device memory twice, so "I do not know what is running" must be an error.
func TestEveryManagerPathReportsABrokenRegistry(t *testing.T) {
	reg := brokenRegistry(t)
	proc := newFakeProc(4242)
	m := managerWith(t, reg, &fakeLauncher{proc: proc}, alwaysHealthy, func(int) error { return nil })

	if _, err := m.Ensure(context.Background(), EnsureConfig{
		ModelID: "m", Runtime: "llama.cpp", Plan: samplePlan(8080),
	}); err == nil {
		t.Error("Ensure must fail when the registry cannot be read")
	}
	if _, err := m.EnsureContainer(context.Background(), ContainerEnsureConfig{
		ModelID: "m", Runtime: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Port: 8000,
	}); err == nil {
		t.Error("EnsureContainer must fail when the registry cannot be read")
	}
	if _, err := m.Status(context.Background()); err == nil {
		t.Error("Status must fail when the registry cannot be read")
	}
	if _, err := m.Stop("m"); err == nil {
		t.Error("Stop must fail when the registry cannot be read")
	}
}

// TestEnsureReportsAFailedRecord checks a server that started and became ready but could not
// be recorded is stopped rather than left running unrecorded: an unrecorded server is one
// nothing can ever stop.
func TestEnsureReportsAFailedRecord(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	unwritable := NewRegistry(filepath.Join(file, "sub"))

	proc := newFakeProc(4242)
	m := managerWith(t, unwritable, &fakeLauncher{proc: proc}, alwaysHealthy, func(int) error { return nil })

	if _, err := m.Ensure(context.Background(), EnsureConfig{
		ModelID: "m", Runtime: "llama.cpp", Plan: samplePlan(8080),
	}); err == nil {
		t.Fatal("Ensure must fail when the server cannot be recorded")
	}
	if !proc.wasStopped() {
		t.Error("a server that could not be recorded was left running")
	}
}

// TestEnsureContainerReportsAFailedRecord is the container counterpart: a container that
// cannot be recorded is torn down, never leaked.
func TestEnsureContainerReportsAFailedRecord(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	unwritable := NewRegistry(filepath.Join(file, "sub"))

	serving := newFakeServing("c1", "docker")
	m := managerWith(t, unwritable, &fakeLauncher{}, alwaysHealthy, func(int) error { return nil },
		withContainerRunner(containerRunner(serving, nil)))

	if _, err := m.EnsureContainer(context.Background(), ContainerEnsureConfig{
		ModelID: "m", Runtime: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Port: 8000,
	}); err == nil {
		t.Fatal("EnsureContainer must fail when the container cannot be recorded")
	}
	if !serving.wasStopped() {
		t.Error("a container that could not be recorded was left running")
	}
}

// TestEnsureContainerRefusesAnIncompletePlan checks the container launch validates its inputs
// before it runs anything.
func TestEnsureContainerRefusesAnIncompletePlan(t *testing.T) {
	var ran bool
	m := managerWith(t, NewRegistry(t.TempDir()), &fakeLauncher{}, alwaysHealthy,
		func(int) error { return nil },
		withContainerRunner(containerRunner(newFakeServing("c1", "docker"), &ran)))

	cases := []ContainerEnsureConfig{
		{Runtime: "vllm", BaseURL: "http://127.0.0.1:8000/v1"}, // no model id
		{ModelID: "m", Runtime: "vllm"},                        // no endpoint
	}
	for _, cfg := range cases {
		if _, err := m.EnsureContainer(context.Background(), cfg); err == nil {
			t.Errorf("EnsureContainer(%+v) must be refused", cfg)
		}
	}
	if ran {
		t.Error("a refused container plan started a container anyway")
	}
}

// TestEnsureContainerReportsAFailedStart checks an engine that cannot run the container is
// reported rather than waited on.
func TestEnsureContainerReportsAFailedStart(t *testing.T) {
	m := managerWith(t, NewRegistry(t.TempDir()), &fakeLauncher{}, alwaysHealthy,
		func(int) error { return nil },
		withContainerRunner(func(context.Context, sandbox.ContainerSpec) (sandbox.Serving, error) {
			return nil, errors.New("no such image")
		}))

	_, err := m.EnsureContainer(context.Background(), ContainerEnsureConfig{
		ModelID: "m", Runtime: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Port: 8000,
	})
	if err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("EnsureContainer error = %v, want the engine's failure", err)
	}
}

// TestEnsureContainerStopsAContainerThatNeverAnswers checks a container that comes up but
// never serves is torn down and reported with its own log tail, so a launch never leaks one.
func TestEnsureContainerStopsAContainerThatNeverAnswers(t *testing.T) {
	serving := newFakeServing("c1", "docker")
	m := managerWith(t, NewRegistry(t.TempDir()), &fakeLauncher{}, neverHealthy,
		func(int) error { return nil },
		withContainerRunner(containerRunner(serving, nil)))

	_, err := m.EnsureContainer(context.Background(), ContainerEnsureConfig{
		ModelID: "m", Runtime: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Port: 8000,
		ReadyTimeout: 20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a container that never answers must fail the launch")
	}
	if !serving.wasStopped() {
		t.Error("a container that never answered was left running")
	}
	if !strings.Contains(err.Error(), "container log tail") {
		t.Errorf("error = %v, want it to carry the container's output", err)
	}
}

// TestWaitReadyHonoursCancellation checks a cancelled context ends the wait rather than
// holding the caller until the readiness deadline.
func TestWaitReadyHonoursCancellation(t *testing.T) {
	m := managerWith(t, NewRegistry(t.TempDir()), &fakeLauncher{proc: newFakeProc(1)},
		neverHealthy, func(int) error { return nil }, WithReadyTimeout(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.waitReady(ctx, newFakeProc(1), "http://127.0.0.1:9/v1", time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady error = %v, want the cancellation", err)
	}
}

// TestStopReportsAFailingKill checks a server that cannot be killed keeps its record: a
// record dropped for a process still holding device memory is a leak nothing can find again.
func TestStopReportsAFailingKill(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	if err := reg.Put(Record{ModelID: "m", PID: 4242, BaseURL: "http://127.0.0.1:8080/v1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	m := managerWith(t, reg, &fakeLauncher{}, alwaysHealthy,
		func(int) error { return errors.New("permission denied") })

	if _, err := m.Stop("m"); err == nil {
		t.Fatal("Stop must report a kill it could not perform")
	}
	if _, ok, err := reg.Get("m"); err != nil || !ok {
		t.Error("the record was dropped even though the server was not stopped")
	}
}

// TestStopOfAnUnknownModelIsNotAnError checks stopping a model that is not running reports
// that nothing was stopped, rather than failing: a redundant stop is safe.
func TestStopOfAnUnknownModelIsNotAnError(t *testing.T) {
	m := managerWith(t, NewRegistry(t.TempDir()), &fakeLauncher{}, alwaysHealthy,
		func(int) error { return errors.New("must not be called") })

	stopped, err := m.Stop("never-started")
	if err != nil || stopped {
		t.Fatalf("Stop = %v, %v, want no server found and no error", stopped, err)
	}
}

// TestTeardownOfAContainerWithoutAStopperIsANoOp checks a container-backed record left by an
// earlier invocation, on a manager with no container stopper wired, is dropped rather than
// failing every stop. The production wiring always supplies a stopper; this is the
// degraded case, and it must not error.
func TestTeardownOfAContainerWithoutAStopperIsANoOp(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	if err := reg.Put(Record{
		ModelID: "m", BaseURL: "http://127.0.0.1:8000/v1", ContainerID: "c1", Engine: "docker",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	m := managerWith(t, reg, &fakeLauncher{}, alwaysHealthy, func(int) error {
		t.Error("a container-backed record must not be killed by pid")
		return nil
	})

	stopped, err := m.Stop("m")
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v, want the record stopped and removed", stopped, err)
	}
	if _, ok, err := reg.Get("m"); err != nil || ok {
		t.Error("the record survived a stop")
	}
}

// TestAContainerStopperIsUsedWhenWired checks the wired stopper is what tears a container
// record down, with the engine and id the record carries.
func TestAContainerStopperIsUsedWhenWired(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	if err := reg.Put(Record{
		ModelID: "m", BaseURL: "http://127.0.0.1:8000/v1", ContainerID: "c1", Engine: "podman",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var gotEngine, gotID string
	m := managerWith(t, reg, &fakeLauncher{}, alwaysHealthy, func(int) error { return nil },
		WithContainerStopper(func(engine, id string) error {
			gotEngine, gotID = engine, id
			return nil
		}),
		WithContainerStopper(nil), // a nil stopper must not unset the one above
	)

	if _, err := m.Stop("m"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if gotEngine != "podman" || gotID != "c1" {
		t.Errorf("stopper called with (%q, %q), want (podman, c1)", gotEngine, gotID)
	}
}

// TestEndpointStopOfAReusedServerIsANoOp checks an adopted endpoint does not stop a server it
// does not own: the owning process, or `models stop`, controls that lifecycle.
func TestEndpointStopOfAReusedServerIsANoOp(t *testing.T) {
	if err := (Endpoint{ModelID: "m", Reused: true}).Stop(); err != nil {
		t.Fatalf("Stop of a reused endpoint = %v, want nil", err)
	}
	proc := newFakeProc(7)
	if err := (Endpoint{ModelID: "m", proc: proc}).Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !proc.wasStopped() {
		t.Error("Stop of an owned endpoint did not stop the process")
	}
}

// TestServingProcHasNoHostPID checks the container adapter reports no pid and tracks the
// container's liveness: identity travels in the record, not as a host process id.
func TestServingProcHasNoHostPID(t *testing.T) {
	serving := newFakeServing("c1", "docker")
	p := servingProc{serving}

	if p.PID() != 0 {
		t.Errorf("PID = %d, want 0: a container has no host pid", p.PID())
	}
	if !p.Running() {
		t.Error("a live container must report as running")
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.Running() {
		t.Error("a stopped container must not report as running")
	}
	if p.Output() == "" {
		t.Error("the container's output must be readable for a failure message")
	}
}

// TestStatsSourceReportsWhatItCannotRead checks the metrics scrape treats a broken endpoint
// as an error for the manager to turn into an unknown reading, rather than as zero load,
// which the scheduler would read as an idle model it may evict.
func TestStatsSourceReportsWhatItCannotRead(t *testing.T) {
	t.Run("a non-2xx reply", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		_, err := LlamaCppStatsSource(srv.Client()).Stats(context.Background(), srv.URL+"/v1")
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("Stats error = %v, want the status", err)
		}
	})

	t.Run("an endpoint that is not there", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		if _, err := VLLMStatsSource(nil).Stats(context.Background(), url+"/v1"); err == nil {
			t.Fatal("a scrape of a dead endpoint must fail")
		}
	})
}

// TestStatsSourceReadsTheRuntimeDialect checks the scrape maps a runtime's own metric names
// onto the snapshot, and that a body naming none of them is a reading of unknown rather than
// a fabricated zero.
func TestStatsSourceReadsTheRuntimeDialect(t *testing.T) {
	body := "llamacpp:requests_processing 2\nllamacpp:requests_deferred 5\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/metrics") {
			t.Errorf("scraped %q, want the metrics endpoint", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	got, err := LlamaCppStatsSource(srv.Client()).Stats(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !got.Known || got.RequestsRunning != 2 || got.RequestsWaiting != 5 {
		t.Errorf("Stats = %+v, want 2 running and 5 waiting", got)
	}

	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# nothing this runtime knows about\n"))
	}))
	t.Cleanup(quiet.Close)
	got, err = LlamaCppStatsSource(quiet.Client()).Stats(context.Background(), quiet.URL+"/v1")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.Known {
		t.Errorf("Stats = %+v, want an unknown reading from a body naming no metric", got)
	}
}

// TestHTTPProbeReportsReadiness checks the production readiness probe: a 2xx from the model
// list means ready, and anything else (a status, a dead endpoint, an address that is not a
// URL) means not ready rather than ready-by-default.
func TestHTTPProbeReportsReadiness(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("probed %q, want the model list", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(ready.Close)
	if err := HTTPProbe(ready.Client())(context.Background(), ready.URL+"/v1"); err != nil {
		t.Fatalf("a 2xx model list must read as ready: %v", err)
	}

	loading := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(loading.Close)
	if err := HTTPProbe(loading.Client())(context.Background(), loading.URL+"/v1"); err == nil {
		t.Error("a 503 must not read as ready")
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	if err := HTTPProbe(nil)(context.Background(), url+"/v1"); err == nil {
		t.Error("an endpoint that is not there must not read as ready")
	}
	if err := HTTPProbe(nil)(context.Background(), "http://127.0.0.1:1/%zz"); err == nil {
		t.Error("an address that is not a URL must not read as ready")
	}
}

// TestOSKillerReportsAProcessItCannotSignal checks the by-pid killer surfaces a failure
// rather than reporting a server as stopped when it is not.
func TestOSKillerReportsAProcessItCannotSignal(t *testing.T) {
	// A pid the OS will not let us signal: no process has ever had it in this test's
	// lifetime, so the kill cannot land.
	if err := OSKiller(-1); err == nil {
		t.Fatal("killing a pid that cannot exist must report a failure")
	}
}
