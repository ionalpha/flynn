package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// configuredRuntime points the microVM configuration knobs at a runtime binary and a guest
// image that exist, and returns their paths. It is how a test drives the operator-facing
// contract (the environment knobs) without a hypervisor.
func configuredRuntime(t *testing.T) (rt, kernel, rootfs string) {
	t.Helper()
	dir := t.TempDir()
	rt = filepath.Join(dir, "runtime")
	kernel = filepath.Join(dir, "vmlinuz")
	rootfs = filepath.Join(dir, "rootfs.img")
	for _, p := range []string{rt, kernel, rootfs} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	t.Setenv(envRuntime, rt)
	t.Setenv(envKernel, kernel)
	t.Setenv(envRootFS, rootfs)
	return rt, kernel, rootfs
}

// hardwareOK is a platform detection function reporting the hardware boundary is present,
// so a test can isolate the configuration half of detection.
func hardwareOK() Availability { return Availability{OK: true, Detail: "kvm is present"} }

// TestCommandDriverDetectRequiresHardwareAndConfiguration proves detection reports
// unavailable, with the reason, for every gap: no hardware boundary, no runtime, a runtime
// that is missing or is a directory, and a guest image that is unset or missing. An
// unavailable tier means untrusted work is refused, never downgraded, so each gap has to be
// named rather than guessed at.
func TestCommandDriverDetectRequiresHardwareAndConfiguration(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		hardware func() Availability
		runtime  string
		kernel   string
		rootfs   string
		wantOK   bool
		want     string
	}{
		{
			name:     "no hardware boundary",
			hardware: func() Availability { return Availability{OK: false, Detail: "kvm is not present"} },
			runtime:  present, kernel: present, rootfs: present,
			want: "kvm is not present",
		},
		{
			name: "no runtime configured", hardware: hardwareOK,
			want: "no microVM runtime configured",
		},
		{
			name: "runtime not found", hardware: hardwareOK,
			runtime: filepath.Join(dir, "absent"),
			want:    "was not found",
		},
		{
			name: "runtime is a directory", hardware: hardwareOK,
			runtime: dir,
			want:    "was not found",
		},
		{
			name: "no guest image", hardware: hardwareOK,
			runtime: present,
			want:    "no guest image configured",
		},
		{
			name: "kernel not found", hardware: hardwareOK,
			runtime: present, kernel: filepath.Join(dir, "absent"), rootfs: present,
			want: "guest kernel",
		},
		{
			name: "root filesystem not found", hardware: hardwareOK,
			runtime: present, kernel: present, rootfs: filepath.Join(dir, "absent"),
			want: "root filesystem",
		},
		{
			name: "fully configured", hardware: hardwareOK,
			runtime: present, kernel: present, rootfs: present,
			wantOK: true, want: "configured",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(envRuntime, c.runtime)
			t.Setenv(envKernel, c.kernel)
			t.Setenv(envRootFS, c.rootfs)
			d := commandDriver{name: "kvm", hardware: c.hardware}
			if d.Name() != "kvm" {
				t.Fatalf("driver name = %q", d.Name())
			}
			av := d.Detect()
			if av.OK != c.wantOK {
				t.Fatalf("availability = %+v, want OK=%v", av, c.wantOK)
			}
			if !strings.Contains(av.Detail, c.want) {
				t.Fatalf("detail %q should say %q", av.Detail, c.want)
			}
		})
	}
}

// TestFileExistsRejectsNonFiles proves the configuration check accepts only a real file: an
// unset path and a directory named where a binary or an image belongs are misconfigurations,
// caught at detection rather than as an opaque failure at launch.
func TestFileExistsRejectsNonFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "runtime")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fileExists("") {
		t.Fatal("an unset path names no file")
	}
	if fileExists(dir) {
		t.Fatal("a directory is not a runtime binary or a guest image")
	}
	if fileExists(filepath.Join(dir, "absent")) {
		t.Fatal("a path that does not exist names no file")
	}
	if !fileExists(file) {
		t.Fatal("a real file must be found")
	}
}

// TestCommandDriverBootRefusesWhenUnconfigured proves a boot on a host with no runtime
// configured is refused as a policy failure rather than falling back to a weaker tier.
func TestCommandDriverBootRefusesWhenUnconfigured(t *testing.T) {
	t.Setenv(envRuntime, "")
	t.Setenv(envKernel, "")
	t.Setenv(envRootFS, "")
	d := commandDriver{name: "kvm", hardware: hardwareOK}
	m, err := d.Boot(context.Background(), untrustedSpec(t.TempDir()))
	if err == nil {
		_ = m.Close()
		t.Fatal("an unconfigured host must refuse to boot a guest")
	}
	if !errors.Is(err, ErrNoMicroVM) {
		t.Fatalf("expected the no-microVM refusal, got %v", err)
	}
}

// TestCommandDriverBootDefaultsTheConfiguredImage proves a caller that names no image boots
// on the operator-configured guest image, the common path, rather than being refused.
func TestCommandDriverBootDefaultsTheConfiguredImage(t *testing.T) {
	rt, kernel, rootfs := configuredRuntime(t)
	d := commandDriver{name: "kvm", hardware: hardwareOK}
	spec := Spec{Root: t.TempDir(), Guarantees: Untrusted(Limits{MemMiB: 512})}
	m, err := d.Boot(context.Background(), spec)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	cm, ok := m.(*commandMachine)
	if !ok {
		t.Fatalf("expected a command machine, got %T", m)
	}
	if cm.runtime != rt {
		t.Fatalf("machine runtime = %q, want the configured runtime %q", cm.runtime, rt)
	}
	if cm.spec.Image.Kernel != kernel || cm.spec.Image.RootFS != rootfs {
		t.Fatalf("machine image = %+v, want the configured guest image", cm.spec.Image)
	}
}

// TestBootMicroVMWithRefusesANilDriver proves the explicit-driver entry point refuses rather
// than panicking or booting nothing when handed no backend.
func TestBootMicroVMWithRefusesANilDriver(t *testing.T) {
	vm, err := BootMicroVMWith(context.Background(), nil, untrustedSpec(t.TempDir()))
	if !errors.Is(err, ErrNoMicroVM) {
		t.Fatalf("expected the no-microVM refusal, got %v (vm=%v)", err, vm)
	}
}

// TestMicroVMServeAndGlobRunInTheGuest proves the tier's server and listing paths go through
// the guest machine and stay confined: a glob matches guest entries by pattern, and a walk or
// read that escapes the guest root is denied before it reaches the machine.
func TestMicroVMServeAndGlobRunInTheGuest(t *testing.T) {
	fm := newFakeMachine()
	fm.listResp = []string{"model.gguf", "notes.txt"}
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}, mach: fm}
	vm, err := BootMicroVMWith(context.Background(), d, untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	ctx := context.Background()

	s, err := vm.Serve(ctx, []string{"server", "--listen"})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if s.Addr() == "" {
		t.Fatal("a served guest process must report its host address")
	}
	_ = s.Stop()

	got, err := vm.Glob(ctx, "*.gguf")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(got) != 1 || got[0] != "model.gguf" {
		t.Fatalf("glob should match only the pattern, got %v", got)
	}
	none, err := vm.Glob(ctx, "*.bin")
	if err != nil || len(none) != 0 {
		t.Fatalf("a pattern matching nothing yields nothing, got %v err=%v", none, err)
	}

	if _, err := vm.Walk(ctx, "../escape"); !errors.Is(err, ErrDenied) {
		t.Fatalf("an escaping walk must be denied, got %v", err)
	}
	if _, err := vm.ReadFile(ctx, "../escape"); !errors.Is(err, ErrDenied) {
		t.Fatalf("an escaping read must be denied, got %v", err)
	}
}

// TestCommandMachineFileAccessGoesThroughTheWorkingArea proves the guest's file operations
// read and list the host working directory the runtime mounts into the guest, so a caller
// reads back what the guest wrote.
func TestCommandMachineFileAccessGoesThroughTheWorkingArea(t *testing.T) {
	root := t.TempDir()
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(root))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	ctx := context.Background()

	if err := cm.WriteFile(ctx, "weights.bin", []byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := cm.ReadFile(ctx, "weights.bin")
	if err != nil || string(got) != "payload" {
		t.Fatalf("read back = %q err=%v", got, err)
	}
	list, err := cm.List(ctx, ".")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0] != "weights.bin" {
		t.Fatalf("list should report the guest working area, got %v", list)
	}
	if _, err := cm.ReadFile(ctx, "../escape"); !errors.Is(err, ErrDenied) {
		t.Fatalf("an escaping guest read must be denied, got %v", err)
	}
}

// TestCommandMachineRefusesWhenTheManifestCannotBeWritten proves a machine whose private
// control directory is gone fails the run and the serve rather than booting a guest with no
// manifest, which would be a guest with no posture at all.
func TestCommandMachineRefusesWhenTheManifestCannotBeWritten(t *testing.T) {
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	if err := os.RemoveAll(cm.control); err != nil {
		t.Fatalf("remove control dir: %v", err)
	}
	if _, err := cm.Exec(context.Background(), "echo hi"); err == nil {
		t.Fatal("a run whose control files cannot be written must fail, never boot an unconfigured guest")
	}
	if s, err := cm.Serve(context.Background(), []string{"server"}); err == nil {
		_ = s.Stop()
		t.Fatal("a serve whose manifest cannot be written must fail, never boot an unconfigured guest")
	}
}

// TestCommandMachineServingReportsOutputAndLifecycle proves a background guest server's
// handle carries the runtime's retained output and a done channel that closes when the guest
// is stopped, so a host can supervise it.
func TestCommandMachineServingReportsOutputAndLifecycle(t *testing.T) {
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	t.Setenv(helperRuntimeEnv, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := cm.Serve(ctx, []string{"server", "--listen"})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(s.Output(), addrPrefix) {
		t.Fatalf("the retained output should carry the runtime's handshake, got %q", s.Output())
	}
	select {
	case <-s.Done():
		t.Fatal("a running guest server must not report done")
	default:
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a stopped guest server must close its done channel")
	}
}

// TestCommandMachineServeAfterCloseStopsRatherThanLeaks proves a Serve that races Close does
// not leave an orphan guest process behind: the machine is closed, so the server it started
// is stopped and the call refuses.
func TestCommandMachineServeAfterCloseStopsRatherThanLeaks(t *testing.T) {
	cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	if err := cm.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := cm.Serve(context.Background(), []string{"server"}); err == nil {
		t.Fatal("serving on a closed machine must be refused")
	}
	// Close is idempotent: a second teardown of an already-closed machine is a no-op.
	if err := cm.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestMicroVMSurfacesGuestListingFailures proves a guest whose listing fails surfaces the
// error on both the glob and the walk, rather than reporting an empty directory that a caller
// would read as "the file is not there".
func TestMicroVMSurfacesGuestListingFailures(t *testing.T) {
	fm := newFakeMachine()
	fm.listErr = errors.New("guest agent is gone")
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}, mach: fm}
	vm, err := BootMicroVMWith(context.Background(), d, untrustedSpec(t.TempDir()))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	if _, err := vm.Glob(context.Background(), "*"); err == nil {
		t.Fatal("a glob whose guest listing failed must surface the error")
	}
	if _, err := vm.Walk(context.Background(), "."); err == nil {
		t.Fatal("a walk whose guest listing failed must surface the error")
	}
	// A write that escapes the guest root is denied before it reaches the machine.
	if err := vm.WriteFile(context.Background(), "../escape", []byte("x")); !errors.Is(err, ErrDenied) {
		t.Fatalf("an escaping guest write must be denied, got %v", err)
	}
}

// TestBootMicroVMRefusesWithNoBackendRegistered proves the selection entry point refuses when
// the binary carries no usable backend, so untrusted work is never run somewhere weaker.
func TestBootMicroVMRefusesWithNoBackendRegistered(t *testing.T) {
	restore := swapDrivers()
	t.Cleanup(restore)
	if _, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir())); !errors.Is(err, ErrNoMicroVM) {
		t.Fatalf("expected the no-microVM refusal, got %v", err)
	}
}

// TestCommandMachineRefusesAnUnusableRuntime proves the two ways a host runtime can fail to
// deliver a guest are errors, never a pretended result: a runtime binary that cannot be run at
// all, and one that runs but reports no guest result back.
func TestCommandMachineRefusesAnUnusableRuntime(t *testing.T) {
	t.Run("runtime cannot be run", func(t *testing.T) {
		cm, err := newCommandMachine(filepath.Join(t.TempDir(), "no-such-runtime"), untrustedSpec(t.TempDir()))
		if err != nil {
			t.Fatalf("new machine: %v", err)
		}
		t.Cleanup(func() { _ = cm.Close() })
		if _, err := cm.Exec(context.Background(), "echo hi"); err == nil {
			t.Fatal("a runtime that cannot be run must fail the guest run")
		}
		if s, err := cm.Serve(context.Background(), []string{"server"}); err == nil {
			_ = s.Stop()
			t.Fatal("a runtime that cannot be run must fail the guest serve")
		}
	})
	t.Run("runtime reports no result", func(t *testing.T) {
		cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
		if err != nil {
			t.Fatalf("new machine: %v", err)
		}
		t.Cleanup(func() { _ = cm.Close() })
		t.Setenv(helperRuntimeEnv, "silent")
		if _, err := cm.Exec(context.Background(), "echo hi"); err == nil {
			t.Fatal("a runtime that reports no guest result must fail, not report an empty success")
		}
	})
	t.Run("runtime fails to boot the guest", func(t *testing.T) {
		cm, err := newCommandMachine(os.Args[0], untrustedSpec(t.TempDir()))
		if err != nil {
			t.Fatalf("new machine: %v", err)
		}
		t.Cleanup(func() { _ = cm.Close() })
		t.Setenv(helperRuntimeEnv, "exit")
		_, err = cm.Exec(context.Background(), "echo hi")
		if err == nil {
			t.Fatal("a runtime that dies before the guest comes up must fail the run")
		}
		if !strings.Contains(err.Error(), "guest failed to boot") {
			t.Fatalf("the runtime's own diagnostic should reach the caller, got %v", err)
		}
	})
}

// TestCommandMachineRefusesAGuestWithNoImage proves a spec with no absolute guest image is
// refused at the manifest, because a guest with no kernel or root filesystem is no boundary.
func TestCommandMachineRefusesAGuestWithNoImage(t *testing.T) {
	spec := Spec{Root: t.TempDir(), Guarantees: Untrusted(Limits{MemMiB: 512})}
	cm, err := newCommandMachine(os.Args[0], spec)
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	if _, err := cm.Exec(context.Background(), "echo hi"); err == nil {
		t.Fatal("a guest with no image must be refused")
	}
	if s, err := cm.Serve(context.Background(), []string{"server"}); err == nil {
		_ = s.Stop()
		t.Fatal("a guest with no image must be refused")
	}
}
