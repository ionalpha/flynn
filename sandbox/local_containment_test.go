package sandbox

import "testing"

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
