package conformtest

import "testing"

// hasLine decides whether a case proved anything, so a loose match would let a
// suite that fails on the wrong subtest pass for the right one.
func TestHasLine(t *testing.T) {
	const marker = "--- FAIL: TestX/Foo"
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"exact with elapsed time", "    --- FAIL: TestX/Foo (0.00s)\n", true},
		{"exact at end of line", "--- FAIL: TestX/Foo\n", true},
		{"indented", "\t\t--- FAIL: TestX/Foo (0.01s)", true},
		{"longer sibling name", "    --- FAIL: TestX/FooBar (0.00s)\n", false},
		{"nested subtest of the name", "    --- FAIL: TestX/Foo/inner (0.00s)\n", false},
		{"a pass", "    --- PASS: TestX/Foo (0.00s)\n", false},
		{"the marker quoted mid-line", "log: --- FAIL: TestX/Foo (0.00s)\n", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasLine(tc.out, marker); got != tc.want {
				t.Fatalf("hasLine(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
