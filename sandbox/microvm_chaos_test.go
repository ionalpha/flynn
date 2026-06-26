package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// This file is the chaos-engineering coverage for the microVM tier: it injects faults at
// the boundary (a backend that fails to boot, a guest that errors, hangs, or dies mid-flight,
// a teardown that fails) and asserts the tier degrades and recovers cleanly. The rule under
// test is that a fault never leaves the tier in an unsafe state: it surfaces an error, never
// downgrades, tears down without leaking, and stays correct under concurrency.

// faultyMachine wraps a Machine and injects scripted faults, the unit of chaos for the
// guest boundary.
type faultyMachine struct {
	inner    Machine
	execErr  error
	closeErr error
	serveErr error
	hangExec bool // block Exec until the context is cancelled, modelling a runaway guest
	mu       sync.Mutex
	closeCnt int
}

func (m *faultyMachine) Exec(ctx context.Context, line string) (ExecResult, error) {
	if m.hangExec {
		<-ctx.Done()
		return ExecResult{}, ctx.Err()
	}
	if m.execErr != nil {
		return ExecResult{}, m.execErr
	}
	return m.inner.Exec(ctx, line)
}

func (m *faultyMachine) Serve(ctx context.Context, argv []string) (Serving, error) {
	if m.serveErr != nil {
		return nil, m.serveErr
	}
	return m.inner.Serve(ctx, argv)
}

func (m *faultyMachine) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return m.inner.ReadFile(ctx, p)
}

func (m *faultyMachine) WriteFile(ctx context.Context, p string, data []byte) error {
	return m.inner.WriteFile(ctx, p, data)
}

func (m *faultyMachine) List(ctx context.Context, dir string) ([]string, error) {
	return m.inner.List(ctx, dir)
}

func (m *faultyMachine) Close() error {
	m.mu.Lock()
	m.closeCnt++
	m.mu.Unlock()
	if m.closeErr != nil {
		return m.closeErr
	}
	return m.inner.Close()
}

func (m *faultyMachine) closeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCnt
}

func bootFaulty(t *testing.T, fm *faultyMachine) *MicroVM {
	t.Helper()
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}, mach: fm}
	restore := swapDrivers(d)
	t.Cleanup(restore)
	vm, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	return vm
}

// A guest that errors surfaces the error rather than a partial or pretended success.
func TestChaosExecErrorSurfaces(t *testing.T) {
	fm := &faultyMachine{inner: newFakeMachine(), execErr: errors.New("guest oom-killed")}
	vm := bootFaulty(t, fm)
	defer func() { _ = vm.Close() }()
	if _, err := vm.Exec(context.Background(), Command{Line: "x"}); err == nil {
		t.Fatal("a guest failure must surface as an error")
	}
}

// A runaway guest that never returns must be bounded by the context: the wall-clock and
// cost breaker depend on the tier honoring cancellation instead of blocking forever.
func TestChaosExecHonorsContextCancellation(t *testing.T) {
	fm := &faultyMachine{inner: newFakeMachine(), hangExec: true}
	vm := bootFaulty(t, fm)
	defer func() { _ = vm.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := vm.Exec(ctx, Command{Line: "loop"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected a deadline error from a hung guest, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not return when its context expired (runaway not bounded)")
	}
}

// A failing teardown surfaces its error but stays idempotent and safe to call again.
func TestChaosCloseFaultIsIdempotent(t *testing.T) {
	fm := &faultyMachine{inner: newFakeMachine(), closeErr: errors.New("vm stop timed out")}
	vm := bootFaulty(t, fm)
	if err := vm.Close(); err == nil {
		t.Fatal("a failing teardown should surface its error")
	}
	// Calling Close again must not panic.
	_ = vm.Close()
	if fm.closeCount() < 2 {
		t.Fatalf("Close should reach the machine each call, got %d", fm.closeCount())
	}
}

// A backend that fails every boot is refused consistently, with no panic and no leaked
// guest, even under repeated pressure.
func TestChaosRepeatedBootFailureNeverLeaks(t *testing.T) {
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}, bootErr: errors.New("kvm: device busy")}
	restore := swapDrivers(d)
	defer restore()
	for range 200 {
		vm, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
		if err == nil {
			t.Fatal("a failing boot must not return a usable tier")
		}
		if vm != nil {
			t.Fatal("a failed boot must not leak a MicroVM handle")
		}
	}
}

// The driver registry is touched from selection, registration, and detection at once; it
// must stay race-free. Run with -race to make this meaningful.
func TestChaosDriverRegistryConcurrent(_ *testing.T) {
	restore := swapDrivers(&fakeDriver{name: "seed", av: Availability{OK: true}})
	defer restore()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(3)
		go func() { defer wg.Done(); RegisterDriver(&fakeDriver{name: "x", av: Availability{Detail: "no"}}) }()
		go func() { defer wg.Done(); _ = Drivers() }()
		go func() { defer wg.Done(); _, _, _ = SelectDriver() }()
	}
	wg.Wait()
}

// A serve whose runtime cannot even start surfaces the start failure rather than hanging or
// pretending the server is up.
func TestChaosServeStartFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	cm, err := newCommandMachine(missing, untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	defer func() { _ = cm.Close() }()
	if _, err := cm.Serve(context.Background(), []string{"server"}); err == nil {
		t.Fatal("expected a start failure for a missing runtime")
	}
}

// A serve whose runtime starts but dies before reporting its address is reported as an
// error, not left waiting, and its diagnostics are captured.
func TestChaosServeExitsBeforeReady(t *testing.T) {
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	defer func() { _ = cm.Close() }()
	t.Setenv(helperRuntimeEnv, "exit")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cm.Serve(ctx, []string{"server"}); err == nil {
		t.Fatal("expected an error when the runtime exits before readiness")
	}
}

// A runaway guest is bounded by the caller's context through the real command path: when
// the deadline fires, Exec returns the context error (so a cancellation is distinguishable
// from a broken runtime), not a misclassified terminal failure, and the runtime is killed.
func TestChaosCommandExecTimeoutSurfacesContextError(t *testing.T) {
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	defer func() { _ = cm.Close() }()
	t.Setenv(helperRuntimeEnv, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = cm.Exec(ctx, "HANG forever")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error from a runaway guest, got %v", err)
	}
}

// A runtime that reports its address and then exits in the same instant must not be misread
// as "never came up": the reported address is honored, since a random select between the
// exit and the address must not flip the result.
func TestChaosServeAddrThenExitNotSpuriousFailure(t *testing.T) {
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	defer func() { _ = cm.Close() }()
	t.Setenv(helperRuntimeEnv, "1")

	// Repeat so the addr-vs-exit race is exercised many times; every run must succeed.
	for range 30 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := cm.Serve(ctx, []string{"server", "EXITAFTERADDR"})
		cancel()
		if err != nil {
			t.Fatalf("serve reported a spurious failure for an addr-then-exit runtime: %v", err)
		}
		if s.Addr() == "" {
			t.Fatal("the reported address was lost")
		}
		_ = s.Stop()
	}
}

// Operations on a closed machine are refused, not run, so a torn-down guest cannot be
// driven by a stale handle.
func TestChaosExecAfterClose(t *testing.T) {
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	if err := cm.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := cm.Close(); err != nil {
		t.Fatalf("second close must be a no-op, got %v", err)
	}
	if _, err := cm.Exec(context.Background(), "echo"); err == nil {
		t.Fatal("exec on a closed machine must be refused")
	}
}
