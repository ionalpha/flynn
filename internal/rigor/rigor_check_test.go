package rigor

import (
	"os"
	"path/filepath"
	"testing"
)

// writePkg creates dir/<name>.go (a production file) plus optional test files.
func writePkg(t *testing.T, root, rel, pkgName string, testFiles map[string]string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte("package "+pkgName+"\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range testFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCheckDetectsViolations proves the gate actually catches gaps (so a green
// run on the real tree is meaningful, not vacuous), and that each escape hatch
// works: a property test satisfies the floor, the grandfather list suppresses it,
// fuzz is required only where declared, and the ratchet flags a grandfathered
// package that has started complying.
func TestCheckDetectsViolations(t *testing.T) {
	const mod = "example.com/m"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+mod+"\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// has a rapid property test -> compliant.
	writePkg(t, root, "good", "good", map[string]string{
		"good_test.go": "package good\n\nimport _ \"pgregory.net/rapid\"\n",
	})
	// no property test -> violation (unless grandfathered).
	writePkg(t, root, "bad", "bad", map[string]string{
		"bad_test.go": "package bad\n",
	})
	// grandfathered, no property test -> suppressed.
	writePkg(t, root, "old", "old", map[string]string{
		"old_test.go": "package old\n",
	})
	// grandfathered BUT now has a property test -> ratchet violation.
	writePkg(t, root, "graduated", "graduated", map[string]string{
		"graduated_test.go": "package graduated\n\nimport _ \"pgregory.net/rapid\"\n",
	})
	// fuzz-required, has property but no fuzz target -> violation.
	writePkg(t, root, "needsfuzz", "needsfuzz", map[string]string{
		"needsfuzz_test.go": "package needsfuzz\n\nimport _ \"pgregory.net/rapid\"\n",
	})
	// a main package -> exempt even with no tests.
	writePkg(t, root, "cmd/tool", "main", nil)
	// a *test helper package -> exempt.
	writePkg(t, root, "helpers/footest", "footest", nil)

	pol := Policy{
		Grandfathered: map[string]bool{"old": true, "graduated": true},
		FuzzRequired:  map[string]bool{"needsfuzz": true},
	}
	vs, err := Check(root, mod, pol)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, v := range vs {
		got[v.Pkg] = v.Reason
	}

	mustViolate := []string{mod + "/bad", mod + "/graduated", mod + "/needsfuzz"}
	mustPass := []string{mod + "/good", mod + "/old", mod + "/cmd/tool", mod + "/helpers/footest"}

	for _, p := range mustViolate {
		if _, ok := got[p]; !ok {
			t.Errorf("expected a violation for %s, got none", p)
		}
	}
	for _, p := range mustPass {
		if r, ok := got[p]; ok {
			t.Errorf("unexpected violation for %s: %s", p, r)
		}
	}
	if len(vs) != len(mustViolate) {
		t.Errorf("got %d violations, want %d: %#v", len(vs), len(mustViolate), got)
	}
}

// TestFuzzDetection confirms a real fuzz target satisfies the fuzz requirement.
func TestFuzzDetection(t *testing.T) {
	const mod = "example.com/m"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+mod+"\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePkg(t, root, "p", "p", map[string]string{
		"p_test.go": "package p\n\nimport (\n\t\"testing\"\n\t_ \"pgregory.net/rapid\"\n)\n\nfunc FuzzThing(f *testing.F) { _ = f }\n",
	})
	vs, err := Check(root, mod, Policy{FuzzRequired: map[string]bool{"p": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations (property + fuzz present), got %#v", vs)
	}
}
