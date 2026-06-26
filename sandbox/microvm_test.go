package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// --- test doubles: a guest and a backend without a hypervisor ---

// fakeMachine is an in-test stand-in for a booted guest. It records what the tier asks of
// it and returns scripted results, so the tier's Sandbox port, gate, and lifecycle are
// proven without real virtualization.
type fakeMachine struct {
	mu       sync.Mutex
	written  map[string][]byte
	listResp []string
	closes   int
	execErr  error
	execOut  ExecResult
	serveErr error
}

func newFakeMachine() *fakeMachine { return &fakeMachine{written: map[string][]byte{}} }

func (f *fakeMachine) Exec(_ context.Context, _ string) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execErr != nil {
		return ExecResult{}, f.execErr
	}
	return f.execOut, nil
}

func (f *fakeMachine) Serve(_ context.Context, _ []string) (Serving, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serveErr != nil {
		return nil, f.serveErr
	}
	return &fakeServing{addr: "127.0.0.1:1", done: make(chan struct{})}, nil
}

func (f *fakeMachine) ReadFile(_ context.Context, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.written[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeMachine) WriteFile(_ context.Context, p string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written[p] = data
	return nil
}

func (f *fakeMachine) List(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listResp, nil
}

func (f *fakeMachine) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

type fakeServing struct {
	addr string
	done chan struct{}
}

func (s *fakeServing) Addr() string          { return s.addr }
func (s *fakeServing) Running() bool         { return true }
func (s *fakeServing) Output() string        { return "" }
func (s *fakeServing) Done() <-chan struct{} { return s.done }
func (s *fakeServing) Stop() error           { close(s.done); return nil }

// fakeDriver is a backend whose availability and boot outcome are scripted.
type fakeDriver struct {
	name    string
	av      Availability
	mach    Machine
	bootErr error

	mu     sync.Mutex
	booted int
}

func (d *fakeDriver) Name() string         { return d.name }
func (d *fakeDriver) Detect() Availability { return d.av }
func (d *fakeDriver) Boot(context.Context, Spec) (Machine, error) {
	d.mu.Lock()
	d.booted++
	d.mu.Unlock()
	if d.bootErr != nil {
		return nil, d.bootErr
	}
	if d.mach != nil {
		return d.mach, nil
	}
	return newFakeMachine(), nil
}

func (d *fakeDriver) bootCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.booted
}

// untrustedSpec is a minimal valid spec for an untrusted guest, used across tests. The
// image paths are derived from the (absolute) root so they are absolute on every platform,
// since the manifest builder refuses a non-absolute image path.
func untrustedSpec(root string) Spec {
	return Spec{
		Root:       root,
		Image:      Image{Kernel: filepath.Join(root, "kernel"), RootFS: filepath.Join(root, "rootfs")},
		Guarantees: Untrusted(Limits{MemMiB: 512}),
	}
}

// absTestPath returns an absolute path under a fresh temp root, for mounts in table tests
// that must be absolute on the host platform.
func absTestPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// --- guarantees validation: the composition cannot be weakened ---

func TestGuaranteesValidate(t *testing.T) {
	absW := absTestPath(t, "w")
	cases := []struct {
		name string
		g    Guarantees
		ok   bool
	}{
		{"untrusted ok", Untrusted(Limits{MemMiB: 256}), true},
		{"untrusted with ro mount", Untrusted(Limits{MemMiB: 256}, Mount{HostPath: absW, GuestPath: "/w"}), true},
		{"egress open refused", Guarantees{EgressDenied: false, Limits: Limits{MemMiB: 256}}, false},
		{"no mem cap refused", Guarantees{EgressDenied: true, Limits: Limits{MemMiB: 0}}, false},
		{"writable mount refused", Guarantees{EgressDenied: true, Limits: Limits{MemMiB: 256}, Mounts: []Mount{{HostPath: absW, GuestPath: "/w", ReadOnly: false}}}, false},
		{"relative mount refused", Guarantees{EgressDenied: true, Limits: Limits{MemMiB: 256}, Mounts: []Mount{{HostPath: "rel", GuestPath: "/w", ReadOnly: true}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.g.validate()
			if c.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatal("expected refusal, got nil")
				}
				if fault.Classify(err) != fault.Forbidden {
					t.Fatalf("a weakened-posture refusal must be Forbidden, got %v", fault.Classify(err))
				}
			}
		})
	}
}

// Untrusted must force every host mount read-only regardless of how the caller set it, so a
// writable mount cannot slip into the untrusted posture by construction.
func TestUntrustedForcesReadOnlyMounts(t *testing.T) {
	g := Untrusted(
		Limits{MemMiB: 128},
		Mount{HostPath: "/a", GuestPath: "/a", ReadOnly: false},
		Mount{HostPath: "/b", GuestPath: "/b", ReadOnly: false},
	)
	if !g.EgressDenied {
		t.Fatal("Untrusted must deny egress")
	}
	for _, m := range g.Mounts {
		if !m.ReadOnly {
			t.Fatalf("mount %q must be forced read-only", m.HostPath)
		}
	}
}

// --- selection + refuse: never downgrade ---

func TestSelectDriverPicksAvailable(t *testing.T) {
	unavailable := &fakeDriver{name: "weak", av: Availability{OK: false, Detail: "no virt"}}
	available := &fakeDriver{name: "strong", av: Availability{OK: true, Detail: "kvm"}}
	restore := swapDrivers(unavailable, available)
	defer restore()

	d, av, err := SelectDriver()
	if err != nil {
		t.Fatalf("expected a driver, got %v", err)
	}
	if d.Name() != "strong" || !av.OK {
		t.Fatalf("expected the available driver, got %q", d.Name())
	}
}

func TestSelectDriverRefusesWhenNoneAvailable(t *testing.T) {
	restore := swapDrivers(
		&fakeDriver{name: "a", av: Availability{Detail: "no kvm"}},
		&fakeDriver{name: "b", av: Availability{Detail: "no hyperv"}},
	)
	defer restore()

	_, _, err := SelectDriver()
	if err == nil {
		t.Fatal("expected a refusal when no backend is available")
	}
	if fault.Classify(err) != fault.Forbidden {
		t.Fatalf("refusal must be Forbidden, got %v", fault.Classify(err))
	}
	if !errors.Is(err, ErrNoMicroVM) {
		t.Fatal("refusal must wrap ErrNoMicroVM")
	}
	// The reason for every backend must be surfaced so an operator knows what to fix.
	for _, want := range []string{"no kvm", "no hyperv"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal should name %q; got %q", want, err.Error())
		}
	}
}

func TestSelectDriverRefusesWithNoBackends(t *testing.T) {
	restore := swapDrivers()
	defer restore()
	if _, _, err := SelectDriver(); err == nil {
		t.Fatal("expected a refusal with no registered backends")
	}
}

// --- boot: validates guarantees, refuses unavailable, reports microVM ---

func TestBootMicroVMReportsMicroVMContainment(t *testing.T) {
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}}
	restore := swapDrivers(d)
	defer restore()

	vm, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = vm.Close() }()

	if got := vm.Containment(); got != ContainmentMicroVM {
		t.Fatalf("containment = %v, want microvm", got)
	}
	if vm.Driver() != "kvm" {
		t.Fatalf("driver = %q, want kvm", vm.Driver())
	}
}

// The whole point of the tier: it is the one level the gate admits untrusted work at, and a
// kernel-confined Local is not. This is the microVM cell of the trust-to-tier matrix.
func TestMicroVMAdmitsUntrustedLocalDoesNot(t *testing.T) {
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}}
	restore := swapDrivers(d)
	defer restore()

	vm, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = vm.Close() }()

	if err := Admit(vm, TrustUntrusted); err != nil {
		t.Fatalf("microVM must admit untrusted work: %v", err)
	}
	local, _ := NewLocal(t.TempDir())
	if err := Admit(local, TrustUntrusted); err == nil {
		t.Fatal("a process-jail Local must refuse untrusted work")
	}
}

func TestBootRefusesWeakenedGuarantees(t *testing.T) {
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}}
	restore := swapDrivers(d)
	defer restore()

	spec := untrustedSpec(t.TempDir())
	spec.Guarantees.EgressDenied = false // open the network: must be refused before boot
	if _, err := BootMicroVM(context.Background(), spec); err == nil {
		t.Fatal("expected refusal of an egress-open guest")
	}
	if d.bootCount() != 0 {
		t.Fatal("a weakened guest must be refused before the driver is asked to boot")
	}
}

func TestBootMicroVMWithRefusesUnavailableDriver(t *testing.T) {
	d := &fakeDriver{name: "kvm", av: Availability{OK: false, Detail: "no kvm device"}}
	_, err := BootMicroVMWith(context.Background(), d, untrustedSpec(t.TempDir()))
	if err == nil {
		t.Fatal("expected refusal for an unavailable driver")
	}
	if fault.Classify(err) != fault.Forbidden {
		t.Fatalf("refusal must be Forbidden, got %v", fault.Classify(err))
	}
	if d.bootCount() != 0 {
		t.Fatal("an unavailable driver must not be booted")
	}
}

func TestBootSurfacesDriverBootError(t *testing.T) {
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}, bootErr: errors.New("kvm: out of memory")}
	restore := swapDrivers(d)
	defer restore()
	_, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("expected the driver boot error surfaced, got %v", err)
	}
}

// --- Sandbox port: default-deny path confinement on top of the guest boundary ---

func TestMicroVMConfinesFileOps(t *testing.T) {
	fm := newFakeMachine()
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}, mach: fm}
	restore := swapDrivers(d)
	defer restore()

	vm, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = vm.Close() }()
	ctx := context.Background()

	// An escaping path is denied before it reaches the guest.
	for _, bad := range []string{"../escape", "/etc/passwd", ".."} {
		if err := vm.WriteFile(ctx, bad, []byte("x")); !errors.Is(err, ErrDenied) {
			t.Fatalf("write %q: expected ErrDenied, got %v", bad, err)
		}
		if _, err := vm.ReadFile(ctx, bad); !errors.Is(err, ErrDenied) {
			t.Fatalf("read %q: expected ErrDenied, got %v", bad, err)
		}
		if _, err := vm.Walk(ctx, bad); !errors.Is(err, ErrDenied) {
			t.Fatalf("walk %q: expected ErrDenied, got %v", bad, err)
		}
	}
	// A confined path reaches the guest as a clean relative path.
	if err := vm.WriteFile(ctx, "out/result.txt", []byte("ok")); err != nil {
		t.Fatalf("write confined: %v", err)
	}
	if _, ok := fm.written["out/result.txt"]; !ok {
		t.Fatalf("confined write did not reach the guest: %v", fm.written)
	}
}

func TestMicroVMServeRejectsEmptyArgv(t *testing.T) {
	fm := newFakeMachine()
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}, mach: fm}
	restore := swapDrivers(d)
	defer restore()
	vm, _ := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
	defer func() { _ = vm.Close() }()
	if _, err := vm.Serve(context.Background(), nil); err == nil {
		t.Fatal("expected an error for empty argv")
	}
}

// --- command machine end to end, driven by a helper "runtime" (no hypervisor needed) ---

// helperRuntimeEnv switches the test binary into acting as the microVM runtime, so the
// whole Flynn side (manifest build, exec, result parse, serve address handshake) is proven
// against a real child process without a hypervisor.
const helperRuntimeEnv = "FLYNN_MICROVM_TEST_HELPER"

// runHelperRuntime acts as the host's microVM runtime: it reads the manifest Flynn wrote,
// and either runs a one-shot command (writing a result file with the guest output and a
// propagated exit code) or serves (announcing the forwarded address, then blocking until
// killed). It faithfully exercises the Flynn-side protocol without booting a real guest.
// It enforces the manifest's load-bearing invariants too, so a regression that weakened the
// posture (egress left open, a writable mount) is caught here rather than passed to a guest.
func runHelperRuntime() {
	// "exit" mode models a runtime that dies before bringing the guest up, for the
	// serve-before-ready chaos path.
	if os.Getenv(helperRuntimeEnv) == "exit" {
		_, _ = os.Stderr.WriteString("runtime: guest failed to boot\n")
		os.Exit(5)
	}
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1]) //nolint:gosec // the manifest path is supplied by Flynn
	if err != nil {
		os.Exit(2)
	}
	var man manifest
	if err := json.Unmarshal(data, &man); err != nil {
		os.Exit(2)
	}
	// A runtime that received a weakened posture refuses, the last line of defense.
	if man.Egress {
		_, _ = os.Stderr.WriteString("runtime: refusing a guest with egress allowed\n")
		os.Exit(4)
	}
	for _, mt := range man.Mounts {
		if !mt.ReadOnly {
			_, _ = os.Stderr.WriteString("runtime: refusing a writable host mount\n")
			os.Exit(4)
		}
	}
	if man.Serve {
		_, _ = os.Stdout.WriteString(addrPrefix + "127.0.0.1:48211\n")
		// "EXITAFTERADDR" models a runtime that reports its address and then exits in the same
		// instant, the case that must not be misread as "never came up".
		if strings.Contains(strings.Join(man.Command, " "), "EXITAFTERADDR") {
			os.Exit(0)
		}
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	exit := 0
	line := strings.Join(man.Command, " ")
	// "HANG" models a runaway guest that never returns, so a test can prove the wall-clock
	// or caller deadline bounds it: the runtime blocks until the parent kills it.
	if strings.Contains(line, "HANG") {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if i := strings.Index(line, "EXIT="); i >= 0 {
		_, _ = fmt.Sscanf(line[i+len("EXIT="):], "%d", &exit)
	}
	res := result{Exit: exit, Output: line}
	out, _ := json.Marshal(res)
	if man.ResultTo != "" {
		_ = os.WriteFile(man.ResultTo, out, 0o600)
	}
	os.Exit(0)
}

func TestCommandMachineExec(t *testing.T) {
	root := t.TempDir()
	spec := untrustedSpec(root)
	cm, err := newCommandMachine(os.Args[0], spec)
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	defer func() { _ = cm.Close() }()
	t.Setenv(helperRuntimeEnv, "1")

	res, err := cm.Exec(context.Background(), "echo hello EXIT=7")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7 (propagated from the guest)", res.ExitCode)
	}
	if !strings.Contains(res.Output, "echo hello") {
		t.Fatalf("output did not carry the command: %q", res.Output)
	}
}

func TestCommandMachineServeHandshake(t *testing.T) {
	root := t.TempDir()
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(root))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	defer func() { _ = cm.Close() }()
	t.Setenv(helperRuntimeEnv, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := cm.Serve(ctx, []string{"server", "--listen"})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if s.Addr() == "" {
		t.Fatal("serve did not report a forwarded address")
	}
	if !s.Running() {
		t.Fatal("server should be running")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestCommandMachineFileOpsAreConfined(t *testing.T) {
	root := t.TempDir()
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(root))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	defer func() { _ = cm.Close() }()
	ctx := context.Background()

	if err := cm.WriteFile(ctx, "a.txt", []byte("data")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The write lands in the host working area the guest shares.
	if b, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(b) != "data" {
		t.Fatalf("expected the write in the working area, got %q err=%v", b, err)
	}
	if err := cm.WriteFile(ctx, "../escape.txt", []byte("x")); !errors.Is(err, ErrDenied) {
		t.Fatalf("escaping write should be denied, got %v", err)
	}
}
