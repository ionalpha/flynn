package sandbox

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// exitCmd is a shell line that exits with the given code on both supported shells
// (POSIX sh and cmd.exe both honor "exit N").
func exitCmd(code int) string { return "exit " + strconv.Itoa(code) }

// TestBestEffortConfinementFallsBack proves the always-on baseline degrades instead of
// failing when the confinement cannot be set up: with confinement requested as the
// default but its setup forced to fail, the command still runs and returns its real
// result rather than an error. The forced failure stands in for a host that refuses
// the namespace setup (such as one restricting unprivileged user namespaces).
func TestBestEffortConfinementFallsBack(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithDefaultConfinement())
	if err != nil {
		t.Fatal(err)
	}
	sb.readonlyFS = true // ensure a confined attempt is made even where the default left it off
	sb.selfExe = filepath.Join(t.TempDir(), "no-such-binary")

	res, err := sb.Exec(context.Background(), Command{Line: "echo fell-back && " + exitCmd(4)})
	if err != nil {
		t.Fatalf("the default baseline must fall back to the floor, not fail: %v", err)
	}
	if res.ExitCode != 4 || !strings.Contains(res.Output, "fell-back") {
		t.Fatalf("the fallback did not run the command: exit %d\n%s", res.ExitCode, res.Output)
	}
}

// TestExplicitConfinementFailsLoud is the other side: a confinement asked for by name
// (not the default baseline) must fail when it cannot be set up, never silently run
// unconfined. The caller asked for the confinement, so its absence is an error.
func TestExplicitConfinementFailsLoud(t *testing.T) {
	// Where the platform enforces confinement, an explicit request is honored, not
	// refused, so there is nothing to fail loud about; this case is a platform that
	// cannot provide it at all. The forced-setup-failure case on a supported platform
	// is covered separately (see the Linux confinement tests).
	if kernelConfinementSupported() {
		t.Skip("kernel confinement is supported on this platform; an explicit request is honored")
	}
	sb, err := NewLocal(t.TempDir(), WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Exec(context.Background(), Command{Line: "echo nope"}); err == nil {
		t.Fatal("an explicitly requested confinement must fail loudly on a platform that cannot provide it")
	}
}

// TestDefaultConfinementPreservesExitCode guards the secure-by-default baseline
// against the failure mode where applying confinement turns a real command result
// into an error: a command's exit code must come back unchanged whether the host
// enforces the confinement or the baseline falls back to the floor because the kernel
// will not set it up. A non-zero exit is a result, never an error, so a verifier or
// caller reading the exit code sees the truth on every host.
func TestDefaultConfinementPreservesExitCode(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithDefaultConfinement())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{0, 3} {
		res, err := sb.Exec(context.Background(), Command{Line: exitCmd(want)})
		if err != nil {
			t.Fatalf("exit %d: unexpected error: %v", want, err)
		}
		if res.ExitCode != want {
			t.Fatalf("exit %d: got exit code %d\n%s", want, res.ExitCode, res.Output)
		}
	}
}

// TestLocalContainmentReflectsConfinement checks that the reported containment level
// tracks what is actually enforced: a bare Local is a process jail, a fully confined
// Local rises to the kernel-confined level where the platform enforces it, and a
// partial configuration does not claim the higher level. The expectation follows the
// platform predicate so the test states the same truth on every platform: where the
// confinement cannot be enforced, the level must not rise.
func TestLocalContainmentReflectsConfinement(t *testing.T) {
	bare, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := bare.Containment(); got != ContainmentNone {
		t.Fatalf("a bare Local must be a process jail, got %v", got)
	}

	wantFull := ContainmentNone
	if kernelConfinementSupported() {
		wantFull = ContainmentKernel
	}

	full, err := NewLocal(t.TempDir(), WithNetworkDenied(), WithReadOnlyFS(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	if got := full.Containment(); got != wantFull {
		t.Fatalf("a fully confined Local must report %v, got %v", wantFull, got)
	}

	// The one-call preset is equivalent to enabling the three confinements by hand.
	preset, err := NewLocal(t.TempDir(), WithKernelConfinement())
	if err != nil {
		t.Fatal(err)
	}
	if got := preset.Containment(); got != wantFull {
		t.Fatalf("WithKernelConfinement must report %v, got %v", wantFull, got)
	}

	// A partial configuration never claims the kernel-confined level, even where the
	// platform could enforce the full set.
	for _, tc := range []struct {
		name string
		opts []LocalOption
	}{
		{"network+fs only", []LocalOption{WithNetworkDenied(), WithReadOnlyFS()}},
		{"network+seccomp only", []LocalOption{WithNetworkDenied(), WithSeccomp()}},
		{"seccomp only", []LocalOption{WithSeccomp()}},
	} {
		sb, err := NewLocal(t.TempDir(), tc.opts...)
		if err != nil {
			t.Fatal(err)
		}
		if got := sb.Containment(); got != ContainmentNone {
			t.Fatalf("%s must not claim kernel confinement, got %v", tc.name, got)
		}
	}
}
