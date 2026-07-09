package rigor

import (
	"fmt"
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
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+mod+"\n\ngo 1.26\n"), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+mod+"\n\ngo 1.26\n"), 0o644); err != nil {
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

// writePkgSrc creates dir/code.go with the given production source plus optional
// test files, so a case can control what the boundary inference sees.
func writePkgSrc(t *testing.T, root, rel, src string, testFiles map[string]string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range testFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	propTest = "package %s\n\nimport _ \"pgregory.net/rapid\"\n"
	fuzzTest = "package %s\n\nimport (\n\t\"testing\"\n\t_ \"pgregory.net/rapid\"\n)\n\nfunc FuzzThing(f *testing.F) { _ = f }\n"
)

// TestBoundaryDecoderInference pins the rule that makes the fuzz list
// self-policing: a package that reaches a network or host boundary AND decodes
// what comes back must declare a fuzz target, whether or not anyone remembered to
// add it to FuzzRequired. Both halves are load-bearing, so a package that only
// reaches a boundary, or only decodes, is left alone.
func TestBoundaryDecoderInference(t *testing.T) {
	const mod = "example.com/m"

	// Each source is a production file; the name says what the inference should make
	// of it.
	const (
		httpJSONUnmarshal = `package p

import (
	"encoding/json"
	"net/http"
)

func F(r *http.Response) error {
	var v map[string]any
	return json.Unmarshal(nil, &v)
}
`
		httpJSONMarshalOnly = `package p

import (
	"encoding/json"
	"net/http"
)

func F(r *http.Response) ([]byte, error) { return json.Marshal(r.Status) }
`
		jsonNoBoundary = `package p

import "encoding/json"

func F(b []byte) error {
	var v map[string]any
	return json.Unmarshal(b, &v)
}
`
		execAliasedDecoder = `package p

import (
	jsonx "encoding/json"
	"os/exec"
)

func F(c *exec.Cmd) *jsonx.Decoder { return jsonx.NewDecoder(nil) }
`
		httpBlankJSON = `package p

import (
	_ "encoding/json"
	"net/http"
)

func F(r *http.Response) string { return r.Status }
`
	)

	cases := []struct {
		name    string
		src     string
		test    string
		exempt  bool
		violate bool
	}{
		{name: "decodes at a boundary with no fuzz target", src: httpJSONUnmarshal, test: propTest, violate: true},
		{name: "decodes at a boundary with a fuzz target", src: httpJSONUnmarshal, test: fuzzTest},
		{name: "decodes at a boundary but is exempt", src: httpJSONUnmarshal, test: propTest, exempt: true},
		{name: "exempt but has grown a fuzz target", src: httpJSONUnmarshal, test: fuzzTest, exempt: true, violate: true},
		{name: "exempt but no longer decodes at a boundary", src: jsonNoBoundary, test: propTest, exempt: true, violate: true},
		{name: "reaches a boundary but only encodes", src: httpJSONMarshalOnly, test: propTest},
		{name: "decodes but reaches no boundary", src: jsonNoBoundary, test: propTest},
		{name: "decodes a subprocess through an aliased import", src: execAliasedDecoder, test: propTest, violate: true},
		{name: "imports a decoder blankly and never decodes", src: httpBlankJSON, test: propTest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+mod+"\n\ngo 1.26\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			writePkgSrc(t, root, "p", tc.src, map[string]string{"p_test.go": fmt.Sprintf(tc.test, "p")})

			pol := Policy{}
			if tc.exempt {
				pol.BoundaryFuzzExempt = map[string]bool{"p": true}
			}
			vs, err := Check(root, mod, pol)
			if err != nil {
				t.Fatal(err)
			}
			if tc.violate && len(vs) == 0 {
				t.Fatal("expected a violation, got none")
			}
			if !tc.violate && len(vs) != 0 {
				t.Fatalf("expected no violation, got %#v", vs)
			}
		})
	}
}
