package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/netguard"
)

// unsettableConfinement builds a sandbox configured for the always-on baseline whose
// confinement cannot be set up, the shape of a host that refuses the namespace setup. The
// override forces the failure on the Linux launcher, which re-executes this binary to build
// the filesystem confinement; the other platforms apply confinement differently and ignore
// it, so the fallback and the loud-failure paths are exercised on Linux.
func unsettableConfinement(t *testing.T, opts ...LocalOption) *Local {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the selfExe setup-failure override only applies to the Linux launcher")
	}
	l := newTestLocal(t, opts...)
	l.readonlyFS = true // ensure a confined attempt is made even where the default left it off
	l.selfExe = filepath.Join(t.TempDir(), "no-such-binary")
	return l
}

// TestBackgroundLaunchesFallBackToTheFloor proves the always-on baseline degrades to the
// process-jail floor rather than failing when the host cannot establish confinement, on every
// launch path that backgrounds a process. The failed attempt never ran the command, so the
// retry is safe, and the baseline is always-on rather than an explicit request.
func TestBackgroundLaunchesFallBackToTheFloor(t *testing.T) {
	t.Run("serve", func(t *testing.T) {
		l := unsettableConfinement(t, WithDefaultConfinement(),
			WithEnv(map[string]string{"FLYNN_TEST_SERVE_HELPER": "block"}))
		p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}, Confine: true})
		if err != nil {
			t.Fatalf("the baseline must fall back to the floor, not fail: %v", err)
		}
		t.Cleanup(func() { _ = p.Stop() })
		if !p.Running() {
			t.Fatal("the fallback did not start the server")
		}
	})
	t.Run("stream", func(t *testing.T) {
		l := unsettableConfinement(t, WithDefaultConfinement())
		p, err := l.Stream(context.Background(), StreamSpec{
			Argv: helperArgv(),
			Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=1"},
		})
		if err != nil {
			t.Fatalf("the baseline must fall back to the floor, not fail: %v", err)
		}
		if err := p.Wait(); err != nil {
			t.Fatalf("the fallback did not run the command: %v", err)
		}
	})
	t.Run("session", func(t *testing.T) {
		l := unsettableConfinement(t, WithDefaultConfinement())
		s, err := l.Session(context.Background(), SessionSpec{
			Argv:    []string{os.Args[0], "-test.run=TestSessionHelperProcess"},
			Env:     []string{"FLYNN_SANDBOX_SESSION_HELPER=1"},
			Confine: true,
		})
		if err != nil {
			t.Fatalf("the baseline must fall back to the floor, not fail: %v", err)
		}
		if err := s.Stop(); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})
	t.Run("capture", func(t *testing.T) {
		l := unsettableConfinement(t, WithDefaultConfinement())
		res, err := l.Capture(context.Background(), CaptureSpec{
			Argv: helperArgv(),
			Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=1"},
		})
		if err != nil {
			t.Fatalf("the baseline must fall back to the floor, not fail: %v", err)
		}
		if !strings.Contains(res.Output, "line0") {
			t.Fatalf("the fallback did not run the command, got %q", res.Output)
		}
	})
}

// TestExplicitConfinementFailsLoudOnEveryLaunchPath is the other side of the fallback: a
// confinement asked for by name is never silently dropped. A launch that cannot establish it
// fails, so a caller never gets a handle to a process running unconfined under a tier the
// trust gate relied on.
func TestExplicitConfinementFailsLoudOnEveryLaunchPath(t *testing.T) {
	t.Run("serve", func(t *testing.T) {
		l := unsettableConfinement(t, WithReadOnlyFS())
		if p, err := l.Serve(context.Background(), ServeSpec{Argv: []string{os.Args[0]}, Confine: true}); err == nil {
			_ = p.Stop()
			t.Fatal("an explicitly confined server must fail rather than run unconfined")
		}
	})
	t.Run("stream", func(t *testing.T) {
		l := unsettableConfinement(t, WithReadOnlyFS())
		if p, err := l.Stream(context.Background(), StreamSpec{Argv: helperArgv(), Confine: true}); err == nil {
			_ = p.Wait()
			t.Fatal("an explicitly confined stream must fail rather than run unconfined")
		}
	})
	t.Run("session", func(t *testing.T) {
		l := unsettableConfinement(t, WithReadOnlyFS())
		if s, err := l.Session(context.Background(), SessionSpec{Argv: helperArgv(), Confine: true}); err == nil {
			_ = s.Stop()
			t.Fatal("an explicitly confined session must fail rather than run unconfined")
		}
	})
	t.Run("capture", func(t *testing.T) {
		l := unsettableConfinement(t, WithReadOnlyFS())
		if _, err := l.Capture(context.Background(), CaptureSpec{Argv: helperArgv()}); err == nil {
			t.Fatal("an explicitly confined capture must fail rather than run unconfined")
		}
	})
}

// TestKernelConfinementOptionEnablesEveryAxis proves the one-call kernel tier is exactly the
// three options together, and that the resource caps are recorded as asked for. A tier that
// quietly left an axis off would report a confinement it does not have.
func TestKernelConfinementOptionEnablesEveryAxis(t *testing.T) {
	l := newTestLocal(t, WithKernelConfinement(), WithResourceLimits(ResourceLimits{MemoryMiB: 256, MaxProcesses: 8}))
	if !l.denyNetwork || !l.readonlyFS || !l.seccomp {
		t.Fatalf("the kernel tier needs all three axes, got network=%v fs=%v seccomp=%v",
			l.denyNetwork, l.readonlyFS, l.seccomp)
	}
	if !l.resLimits.set() {
		t.Fatal("configured resource caps must report themselves as set")
	}
	if (ResourceLimits{}).set() {
		t.Fatal("the zero value caps nothing, so it must not report itself as set")
	}
}

// TestEgressAndForwardConfigsCloseCleanly proves a sandbox that governs egress and forwards a
// host address releases both on Close even when no child was ever launched, so a closed
// sandbox holds no proxy and no forwarder. Close is idempotent.
func TestEgressAndForwardConfigsCloseCleanly(t *testing.T) {
	l, err := NewLocal(t.TempDir(),
		WithEgress(netguard.PublicOnly()),
		WithLoopbackForward("127.0.0.1:65000"))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if !l.egressActive() {
		t.Fatal("a sandbox built with WithEgress must govern egress")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close must be idempotent: %v", err)
	}
}

// TestWriteFileSurfacesFilesystemRefusals proves a confined write that the filesystem cannot
// satisfy is an error rather than a silent no-op a caller would read as a successful write.
func TestWriteFileSurfacesFilesystemRefusals(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()
	if err := l.WriteFile(ctx, "file.txt", []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A parent directory that is really a file cannot be created.
	if err := l.WriteFile(ctx, "file.txt/child.txt", []byte("x")); err == nil {
		t.Fatal("a write under a path that is a file must fail")
	}
	// The target itself being a directory is refused by the open, not silently ignored.
	if err := os.Mkdir(filepath.Join(l.Root(), "adir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := l.WriteFile(ctx, "adir", []byte("x")); err == nil {
		t.Fatal("writing over a directory must fail")
	}
}

// TestContainmentLevelsAreNamed proves every level renders a distinct name, since the name is
// what a governed run records as the boundary it actually ran under.
func TestContainmentLevelsAreNamed(t *testing.T) {
	want := map[Containment]string{
		ContainmentNone:       "process-jail",
		ContainmentKernel:     "kernel-confined",
		ContainmentContainer:  "container",
		ContainmentUserKernel: "userspace-kernel",
		ContainmentMicroVM:    "microvm",
		ContainmentRemote:     "remote",
	}
	seen := map[string]bool{}
	for level, name := range want {
		got := level.String()
		if got != name {
			t.Fatalf("Containment(%d) = %q, want %q", level, got, name)
		}
		if seen[got] {
			t.Fatalf("two levels render the same name %q", got)
		}
		seen[got] = true
	}
	if got := Containment(99).String(); got == "" {
		t.Fatal("an unknown level must still render something for a record")
	}
}

// stubDriver is a container engine that reports itself available and adopts nothing, for the
// registry and entry-point tests.
type stubDriver struct {
	name string
	av   Availability
	runs int
}

func (d *stubDriver) Name() string         { return d.name }
func (d *stubDriver) Detect() Availability { return d.av }

func (d *stubDriver) Run(_ context.Context, _ ContainerSpec) (Serving, error) {
	d.runs++
	return &fakeServing{addr: "127.0.0.1:8123", done: make(chan struct{})}, nil
}

// TestContainerRegistryIgnoresNilAndSelectsAvailable proves a nil driver is never registered,
// that the entry point runs on the first available engine, and that a host with no usable
// engine is refused rather than downgraded.
func TestContainerRegistryIgnoresNilAndSelectsAvailable(t *testing.T) {
	down := &stubDriver{name: "docker", av: Availability{OK: false, Detail: "daemon is not running"}}
	up := &stubDriver{name: "podman", av: Availability{OK: true, Detail: "podman server 5"}}
	restore := swapContainerDrivers(down, up)
	t.Cleanup(restore)
	RegisterContainerDriver(nil) // a nil driver is ignored, never registered
	if len(ContainerDrivers()) != 2 {
		t.Fatalf("a nil driver must not be registered, got %d drivers", len(ContainerDrivers()))
	}

	s, err := RunContainer(context.Background(), servingSpec())
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	_ = s.Stop()
	if up.runs != 1 || down.runs != 0 {
		t.Fatalf("the container must run on the available engine, got podman=%d docker=%d", up.runs, down.runs)
	}

	// A spec that would weaken the untrusted posture is refused before an engine is selected.
	if _, err := RunContainer(context.Background(), ContainerSpec{}); err == nil {
		t.Fatal("an invalid container spec must be refused")
	}
	if up.runs != 1 {
		t.Fatal("a refused spec must never reach an engine")
	}

	// With no usable engine the tier refuses rather than running the work somewhere weaker.
	restoreNone := swapContainerDrivers(down)
	t.Cleanup(restoreNone)
	if _, err := RunContainer(context.Background(), servingSpec()); !errors.Is(err, ErrNoContainerRuntime) {
		t.Fatalf("expected the no-runtime refusal, got %v", err)
	}
}

// TestMicroVMRegistryIgnoresNil proves a nil microVM backend is never registered, so the
// selection loop cannot dereference one.
func TestMicroVMRegistryIgnoresNil(t *testing.T) {
	d := &fakeDriver{name: "kvm", av: Availability{OK: true}}
	restore := swapDrivers(d)
	t.Cleanup(restore)
	RegisterDriver(nil)
	if len(Drivers()) != 1 {
		t.Fatalf("a nil driver must not be registered, got %d", len(Drivers()))
	}
}
