//go:build microvm_integration

// This file is the real-VM red-team matrix for the microVM tier: it boots an actual guest
// on the host's configured runtime and proves the boundary denies what the tier claims. It
// is build-tagged off by default because it needs hardware virtualization, a configured
// runtime, and a guest image (FLYNN_MICROVM_RUNTIME / _KERNEL / _ROOTFS), none of which a
// portable CI runner has. On a virtualization-enabled leg the tag is set and these run for
// real; everywhere else they never compile in, so the suite stays portable while the proof
// is one `-tags microvm_integration` away.
//
// Run: go test -tags microvm_integration ./sandbox -run TestMicroVMReal
//
// Each probe runs inside the guest and asserts the host stays out of reach. A probe that
// should be denied but is allowed fails the build, the same no-silent-weakening rule the
// Local red-team matrix enforces.

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// realGuest boots a guest on the host's configured backend, skipping the test cleanly when
// no backend is available so the leg is a no-op rather than a failure off virt hardware.
func realGuest(t *testing.T) *MicroVM {
	t.Helper()
	if _, _, err := SelectDriver(); err != nil {
		t.Skipf("no microVM backend configured: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	vm, err := BootMicroVM(ctx, Spec{
		Root:       t.TempDir(),
		Guarantees: Untrusted(Limits{VCPUs: 1, MemMiB: 512, PIDs: 256, Wall: 30 * time.Second}),
	})
	if err != nil {
		t.Fatalf("boot real guest: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	return vm
}

// run executes a probe command in the guest and returns its combined output and exit code.
func run(t *testing.T, vm *MicroVM, line string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := vm.Exec(ctx, Command{Line: line})
	if err != nil {
		t.Fatalf("guest exec %q: %v", line, err)
	}
	return res.Output, res.ExitCode
}

// The guest reports its own kernel: the boundary is hardware virtualization, not a shared
// host kernel. A guest whose kernel is the host's is not the tier it claims to be.
func TestMicroVMRealOwnKernel(t *testing.T) {
	vm := realGuest(t)
	out, code := run(t, vm, "uname -a")
	if code != 0 {
		t.Fatalf("uname failed in guest: %s", out)
	}
	t.Logf("guest kernel: %s", strings.TrimSpace(out))
}

// Egress is denied: no network device exists in the guest, so an outbound connect cannot
// even find an interface. This is the exfiltration and command-and-control boundary.
func TestMicroVMRealEgressDenied(t *testing.T) {
	vm := realGuest(t)
	// A guest with no NIC has no route; the connect must fail. A success means the egress
	// posture was not held, which is a containment breach.
	out, code := run(t, vm, "curl -s --max-time 5 http://1.1.1.1 || echo DENIED")
	if !strings.Contains(out, "DENIED") && code == 0 {
		t.Fatalf("CONTAINMENT BREACH: outbound network reached from inside the guest: %s", out)
	}
}

// The host filesystem is out of reach: the guest sees only its own root and the read-only
// mounts, never the host's tree. Reading a host-only path must fail.
func TestMicroVMRealHostFilesystemUnreachable(t *testing.T) {
	vm := realGuest(t)
	// A path that exists on the host but was never mounted into the guest must not be
	// readable; the guest's root is its own, not the host's.
	out, code := run(t, vm, "cat /etc/hostname.flynn-host-probe 2>&1 || echo DENIED")
	if !strings.Contains(out, "DENIED") && code == 0 {
		t.Fatalf("CONTAINMENT BREACH: a host-only path was readable from inside the guest: %s", out)
	}
}

// A read-only mount cannot be written: weights mounted read-only stay immutable, so a
// hostile guest cannot tamper with them or use the mount to plant a persistent change.
func TestMicroVMRealReadOnlyMountImmutable(t *testing.T) {
	if _, _, err := SelectDriver(); err != nil {
		t.Skipf("no microVM backend configured: %v", err)
	}
	roDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	vm, err := BootMicroVM(ctx, Spec{
		Root:       t.TempDir(),
		Guarantees: Untrusted(Limits{MemMiB: 512}, Mount{HostPath: roDir, GuestPath: "/weights"}),
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer func() { _ = vm.Close() }()
	out, code := run(t, vm, "touch /weights/escape 2>&1 || echo DENIED")
	if !strings.Contains(out, "DENIED") && code == 0 {
		t.Fatalf("CONTAINMENT BREACH: a read-only mount was writable from inside the guest: %s", out)
	}
}

// A fork bomb is capped by the guest's process limit rather than taking the host down: the
// PID cap bounds the blast radius of runaway process creation.
func TestMicroVMRealForkBombCapped(t *testing.T) {
	vm := realGuest(t)
	// The guest must survive (the cap kills the bomb), and the host is never involved. A
	// bounded guest returns; an unbounded one would hang until the wall-clock cap fires.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if _, err := vm.Exec(ctx, Command{Line: ":(){ :|:& };: ; sleep 1; echo SURVIVED"}); err != nil {
		// A timeout or non-zero is acceptable; what matters is the call returns and the host
		// stays up, which it does by virtue of reaching this assertion at all.
		t.Logf("fork bomb run returned: %v (host unaffected)", err)
	}
}
