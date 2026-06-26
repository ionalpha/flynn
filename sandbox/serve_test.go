package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain lets a Serve test re-execute this test binary as a small, cross-platform
// stand-in for a long-lived server: with FLYNN_TEST_SERVE_HELPER set it prints a
// readiness line and blocks until killed, instead of running the test suite. Serve
// scrubs the environment, so a test grants the variable explicitly with WithEnv; that
// is also what proves the grant path reaches a backgrounded process.
func TestMain(m *testing.M) {
	switch os.Getenv("FLYNN_TEST_SERVE_HELPER") {
	case "block":
		// A stand-in server: announce readiness, then run until the parent stops it. A
		// long sleep (not an empty select) keeps a timer goroutine alive so the Go
		// runtime's deadlock detector does not abort the stand-in on its own.
		_, _ = os.Stdout.WriteString("helper-up\n")
		time.Sleep(time.Hour)
		os.Exit(0)
	case "exit":
		// A runtime that fails to come up: print a diagnostic and exit non-zero.
		_, _ = os.Stderr.WriteString("boom: cannot bind\n")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func newServeSandbox(t *testing.T, mode string) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir(), WithEnv(map[string]string{"FLYNN_TEST_SERVE_HELPER": mode}))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return l
}

func TestServeStartsCapturesAndStops(t *testing.T) {
	l := newServeSandbox(t, "block")
	p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if p.PID() == 0 {
		t.Fatal("expected a non-zero pid for a started process")
	}
	// The process announces readiness on its own; wait for the captured tail to show it.
	waitFor(t, time.Second, func() bool { return strings.Contains(p.Output(), "helper-up") })
	if !p.Running() {
		t.Fatal("process should still be running before Stop")
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.Running() {
		t.Fatal("process should not be running after Stop")
	}
	// Stop is idempotent.
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestServeReportsExitThroughHandle(t *testing.T) {
	l := newServeSandbox(t, "exit")
	p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// A process that exits on its own is reported through Wait, not through Serve.
	if err := p.Wait(context.Background()); err == nil {
		t.Fatal("expected a non-nil exit error for a non-zero exit")
	}
	if p.Running() {
		t.Fatal("Running should be false after the process exits")
	}
	if !strings.Contains(p.Output(), "boom: cannot bind") {
		t.Fatalf("expected the failure diagnostic in the captured output, got %q", p.Output())
	}
}

func TestServeWaitHonorsContext(t *testing.T) {
	l := newServeSandbox(t, "block")
	p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer func() { _ = p.Stop() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// The process never exits on its own, so Wait must return the context error and
	// must not have killed the still-running server.
	if err := p.Wait(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Wait = %v, want DeadlineExceeded", err)
	}
	if !p.Running() {
		t.Fatal("a context timeout on Wait must not stop the process")
	}
}

func TestServeRefusesEmptyCommand(t *testing.T) {
	l := newServeSandbox(t, "block")
	if _, err := l.Serve(context.Background(), ServeSpec{Argv: nil}); err == nil {
		t.Fatal("expected an error for an empty command")
	}
	if _, err := l.Serve(context.Background(), ServeSpec{Argv: []string{""}}); err == nil {
		t.Fatal("expected an error for an empty argv[0]")
	}
}

func TestServeFailsToStartUnknownBinary(t *testing.T) {
	l := newServeSandbox(t, "block")
	_, err := l.Serve(context.Background(), ServeSpec{Argv: []string{"flynn-no-such-binary-xyz"}})
	if err == nil {
		t.Fatal("expected a start error for a missing binary")
	}
}

// waitFor polls cond until it holds or the deadline passes, so a test never sleeps a
// fixed duration waiting on a background process's output.
func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", within)
	}
}
