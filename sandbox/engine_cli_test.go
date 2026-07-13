package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// missingEngine names an OCI engine that is certainly not installed, so every path that
// drives the real engine CLI can be exercised for its failure behavior on any host,
// with or without a container runtime present.
const missingEngine OCIEngine = "flynn-no-such-container-engine"

// engineArgv builds a portable engine command out of the platform shell, so the real
// exec runner can be driven to completion on any OS without a container engine.
func engineArgv(line string) []string {
	name, args := shell(line)
	return append([]string{name}, args...)
}

// TestExecRunnerReturnsStdout proves the default runner returns the engine's stdout, which
// is where a container id is read from.
func TestExecRunnerReturnsStdout(t *testing.T) {
	out, err := execRunner(context.Background(), engineArgv("echo flynn-ok"))
	if err != nil {
		t.Fatalf("execRunner: %v", err)
	}
	if !strings.Contains(out, "flynn-ok") {
		t.Fatalf("expected the command's stdout, got %q", out)
	}
}

// TestExecRunnerRefusesEmptyArgv proves an empty engine command is refused rather than
// exec'ing something undefined.
func TestExecRunnerRefusesEmptyArgv(t *testing.T) {
	if _, err := execRunner(context.Background(), nil); err == nil {
		t.Fatal("an empty engine command must be refused")
	}
}

// TestExecRunnerFoldsStderrIntoTheError proves a failed engine command surfaces as an
// error naming the engine and its subcommand, with the engine's diagnostic folded in, so
// the failure is diagnosable rather than an opaque exit code.
func TestExecRunnerFoldsStderrIntoTheError(t *testing.T) {
	_, err := execRunner(context.Background(), engineArgv("echo engine-said-no 1>&2 && exit 3"))
	if err == nil {
		t.Fatal("a failing engine command must be an error")
	}
	if !strings.Contains(err.Error(), "engine-said-no") {
		t.Fatalf("the engine's diagnostic should be folded into the error, got %v", err)
	}
}

// TestExecRunnerReportsAMissingEngine proves an engine binary that is not installed is a
// clear error rather than a panic or an empty success a caller would read as a started
// container.
func TestExecRunnerReportsAMissingEngine(t *testing.T) {
	out, err := execRunner(context.Background(), []string{string(missingEngine)})
	if err == nil {
		t.Fatalf("a missing engine must be an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), string(missingEngine)) {
		t.Fatalf("the error should name the engine, got %v", err)
	}
}

// TestExecRunnerSurfacesContextCancellation proves a cancelled context is reported as the
// context's error, not as an engine failure, so a caller can tell a cancelled run from a
// broken engine.
func TestExecRunnerSurfacesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := execRunner(ctx, engineArgv("echo never"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the context error, got %v", err)
	}
}

// TestEngineLifecycleCallsReportAMissingEngine proves the cross-invocation teardown and
// image/network provisioning calls fail loudly on a host with no engine, rather than
// reporting success for work that never happened.
func TestEngineLifecycleCallsReportAMissingEngine(t *testing.T) {
	ctx := context.Background()
	if err := StopContainer(ctx, missingEngine, fakeCID); err == nil {
		t.Fatal("stopping a container on a missing engine must be an error")
	}
	if err := PullImage(ctx, missingEngine, "ghcr.io/x/y", "sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("pulling on a missing engine must be an error")
	}
	if err := PullImage(ctx, missingEngine, "", "sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("pulling a bare digest on a missing engine must be an error")
	}
	if err := EnsureContainerNetwork(ctx, missingEngine, "flynn-net"); err == nil {
		t.Fatal("creating a network on a missing engine must be an error")
	}
}

// TestEnsureContainerNetworkRefusesAnUnnamedNetwork proves the network name is required, so
// a caller cannot ask for a network the engine would name arbitrarily.
func TestEnsureContainerNetworkRefusesAnUnnamedNetwork(t *testing.T) {
	err := EnsureContainerNetwork(context.Background(), EngineDocker, "")
	if err == nil || !strings.Contains(err.Error(), "needs a name") {
		t.Fatalf("an unnamed serving network must be refused, got %v", err)
	}
}

// TestOCIDriverNamesItsEngine proves the driver reports the engine it drives, which is what
// a record names when it says where a container ran. A nil runner takes the real CLI runner,
// the production wiring.
func TestOCIDriverNamesItsEngine(t *testing.T) {
	if got := NewContainerDriver(EnginePodman, nil).Name(); got != "podman" {
		t.Fatalf("driver name = %q, want podman", got)
	}
	if got := NewContainerDriver(EngineDocker, newFakeEngine().runner).Name(); got != "docker" {
		t.Fatalf("driver name = %q, want docker", got)
	}
}

// TestContainerServingIdentifiesTheContainer proves an adopted container carries the engine
// and the id a later, separate process needs to stop it (a container has no host pid to
// signal), and that its retained logs are readable while it is alive.
func TestContainerServingIdentifiesTheContainer(t *testing.T) {
	f := newFakeEngine()
	s, err := NewContainerDriver(EnginePodman, f.runner).Run(context.Background(), servingSpec())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	cs, ok := s.(*containerServing)
	if !ok {
		t.Fatalf("expected a container serving, got %T", s)
	}
	if cs.ContainerID() != fakeCID {
		t.Fatalf("container id = %q, want the id the engine printed", cs.ContainerID())
	}
	if cs.EngineName() != "podman" {
		t.Fatalf("engine name = %q, want podman", cs.EngineName())
	}
	if !strings.Contains(cs.Output(), "a log line") {
		t.Fatalf("expected the retained container logs, got %q", cs.Output())
	}
	f.release()
	waitClosed(t, cs.Done())
}

// TestContainerServingOutputIsBestEffort proves a container whose logs no longer resolve
// (a --rm container already torn down) reports no output rather than surfacing the engine's
// error as if it were the container's own diagnostics.
func TestContainerServingOutputIsBestEffort(t *testing.T) {
	f := newFakeEngine()
	noLogs := func(ctx context.Context, argv []string) (string, error) {
		if len(argv) > 1 && argv[1] == "logs" {
			return "", errors.New("no such container")
		}
		return f.runner(ctx, argv)
	}
	s := newContainerServing(EngineDocker, fakeCID, "", noLogs)
	t.Cleanup(func() {
		f.release()
		<-s.Done()
	})
	if got := s.Output(); got != "" {
		t.Fatalf("a container with no readable logs should report no output, got %q", got)
	}
}
