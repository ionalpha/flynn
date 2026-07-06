package sandbox

import "testing"

// TestResourceLimitsSet checks the axis-set predicate that gates enforcement: a cap
// on any axis counts as set, and the zero value counts as unset so the always-on
// floor stays out of a legitimate command's way by default.
func TestResourceLimitsSet(t *testing.T) {
	cases := []struct {
		name string
		lim  ResourceLimits
		want bool
	}{
		{"zero", ResourceLimits{}, false},
		{"memory", ResourceLimits{MemoryMiB: 512}, true},
		{"processes", ResourceLimits{MaxProcesses: 16}, true},
		{"both", ResourceLimits{MemoryMiB: 512, MaxProcesses: 16}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lim.set(); got != tc.want {
				t.Errorf("set() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWithResourceLimits proves the option records the caps on the sandbox, and that a
// sandbox constructed without it carries none, so an unconfigured caller is unaffected.
func TestWithResourceLimits(t *testing.T) {
	want := ResourceLimits{MemoryMiB: 1024, MaxProcesses: 32}
	sb, err := NewLocal(t.TempDir(), WithResourceLimits(want))
	if err != nil {
		t.Fatal(err)
	}
	if sb.resLimits != want {
		t.Errorf("resource limits = %+v, want %+v", sb.resLimits, want)
	}

	def, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if def.resLimits.set() {
		t.Errorf("a sandbox with no resource-limit option must carry none, got %+v", def.resLimits)
	}
}
