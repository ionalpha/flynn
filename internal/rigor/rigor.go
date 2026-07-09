// Package rigor enforces the project's engineering-rigor floor as a test rather
// than a hope: every production package must carry a property test (rapid or the
// testkit harness), a declared set must carry a fuzz target, and a declared set
// must carry a benchmark (so a hot path, once measured, can never silently lose
// its measurement). Because the gate
// is an ordinary Go test (see rigor_test.go), it runs inside dev/test, dev/check,
// and the CI test matrix with no extra wiring, so a package added without its
// required tests turns `go test ./...` red locally and in CI.
//
// The fuzz requirement is not only a list. A declared list is a thing someone has
// to remember to add a package to, and the parsers that most needed fuzzing were
// exactly the ones nobody remembered. So the gate also infers it: a package that
// reaches a network or host boundary and decodes what comes back is parsing input
// it does not control, and must carry a fuzz target whether or not it was ever
// listed. A new parser wired to a socket or a subprocess now cannot land without
// one.
//
// New packages are held to the floor immediately. A grandfather allowlist covers
// the gaps that predate the gate so it lands green; the list only ever shrinks
// (the gate fails if a grandfathered package starts complying, forcing its
// removal), and nothing new is added to it. The boundary-fuzz exemption list works
// the same way.
package rigor

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// rapidImport is the property-testing library; importing it (or the testkit
// harness, which is built on it) in a test file satisfies the property-test
// requirement.
const rapidImport = "pgregory.net/rapid"

// Policy parameterizes the gate. Keeping it injectable lets the checker's own
// logic be unit-tested against synthetic trees, while the live gate uses
// DefaultPolicy.
type Policy struct {
	// Grandfathered lists module-relative package paths that predate the gate and
	// have no property test yet. Burn it down; never add to it.
	Grandfathered map[string]bool
	// FuzzRequired lists module-relative packages that parse untrusted input and so
	// must carry a fuzz target.
	FuzzRequired map[string]bool
	// BenchRequired lists module-relative packages on the hot path (the write
	// path, the canonical codec, the durable store) that must carry a benchmark,
	// so dev/bench and the CI bench smoke always have something to measure and a
	// regression gate cannot be deleted along with its benchmark. Grow it as
	// hotspots gain benchmarks; never shrink it.
	BenchRequired map[string]bool
	// BoundaryFuzzExempt lists module-relative packages the boundary-decoder
	// inference flags but which carry no fuzz target yet. It exists so the
	// inference can land green over a tree that predates it. Burn it down; never
	// add to it. The gate fails on an entry that has gained a fuzz target, and on
	// one that no longer decodes at a boundary, so the list can only shrink.
	BoundaryFuzzExempt map[string]bool
}

// boundaryImports are the standard-library packages whose use means bytes the
// process did not produce cross into this one: a network peer, a subprocess.
var boundaryImports = map[string]bool{
	"net":      true,
	"net/http": true,
	"os/exec":  true,
}

// decodeFuncs maps a decoder package to the calls that turn foreign bytes or a
// reader into Go values. A package that both reaches a boundary and calls one of
// these is parsing input it does not control, which is the definition of a fuzz
// target's input.
var decodeFuncs = map[string]map[string]bool{
	"encoding/json": {"Unmarshal": true, "NewDecoder": true},
	"encoding/xml":  {"Unmarshal": true, "NewDecoder": true},
	"encoding/gob":  {"NewDecoder": true},
	"encoding/csv":  {"NewReader": true},
	"encoding/pem":  {"Decode": true},
}

// DefaultPolicy is the policy the live gate enforces. The empty string is the
// root (module) package.
func DefaultPolicy() Policy {
	return Policy{
		// Empty, and it stays empty: every package that predated the gate now
		// carries its property test. The gate fails on an entry whose package has
		// gained one, so the list cannot silently grow back.
		Grandfathered: map[string]bool{},
		FuzzRequired: map[string]bool{
			"bus":                   true,
			"fault":                 true,
			"jobs":                  true,
			"spine":                 true,
			"resource":              true,
			"internal/fetch":        true,
			"netguard":              true,
			"internal/tui/input":    true,
			"internal/tui/mdstream": true,
			"internal/tui/render":   true,
			"internal/tui/theme":    true,
		},
		BenchRequired: map[string]bool{
			"chain":          true,
			"internal/flow":  true,
			"jobs":           true,
			"mission":        true,
			"resource":       true,
			"storage/sqlite": true,
		},
		// Empty, and it stays empty: every package that decodes bytes it does not
		// control now carries a fuzz target. An entry here only ever bought time for
		// a decoder that predated the inference, and the gate fails on one that has
		// since gained a target, so the list cannot silently grow back.
		BoundaryFuzzExempt: map[string]bool{},
	}
}

// Violation is one package failing the rigor floor.
type Violation struct {
	Pkg    string // full import path
	Reason string
}

// Check walks the module rooted at root (with module path modulePath) and returns
// every rigor violation under pol. It reads source only; it does not build or run
// packages.
func Check(root, modulePath string, pol Policy) ([]Violation, error) {
	testkitImport := modulePath + "/internal/testkit"
	var vs []Violation

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && skipDir(d.Name()) {
			return filepath.SkipDir
		}

		pkg, err := build.ImportDir(p, 0)
		if err != nil {
			var noGo *build.NoGoError
			if errors.As(err, &noGo) {
				return nil // no Go files for this build context: not a package
			}
			return fmt.Errorf("rigor: import %s: %w", p, err)
		}
		if len(pkg.GoFiles) == 0 {
			return nil // no production code to hold to the floor
		}

		rel := relPath(root, p)
		if exempt(rel, pkg.Name) {
			return nil
		}
		label := modulePath
		if rel != "" {
			label = modulePath + "/" + rel
		}

		hasProperty := importsAny(pkg.TestImports, rapidImport, testkitImport) ||
			importsAny(pkg.XTestImports, rapidImport, testkitImport)
		gf := pol.Grandfathered[rel]

		switch {
		case !hasProperty && !gf:
			vs = append(vs, Violation{label, "missing a property test: a _test.go must import " + rapidImport + " or the testkit harness"})
		case hasProperty && gf:
			vs = append(vs, Violation{label, "now has a property test: remove it from the rigor grandfather allowlist (the list only shrinks)"})
		}

		testFiles := append(append([]string{}, pkg.TestGoFiles...), pkg.XTestGoFiles...)
		if pol.FuzzRequired[rel] {
			ok, ferr := hasFuncPrefix(p, testFiles, "Fuzz")
			if ferr != nil {
				return ferr
			}
			if !ok {
				vs = append(vs, Violation{label, "missing a fuzz target: declare a func FuzzXxx(*testing.F)"})
			}
		}
		// The declared FuzzRequired list is a hand-maintained allowlist, so a new
		// parser can be added without anyone remembering to list it. Infer the
		// requirement instead: a package that reaches a network or host boundary and
		// decodes what comes back is parsing input it does not control.
		exemptFuzz := pol.BoundaryFuzzExempt[rel]
		boundary, derr := decodesAtBoundary(p, pkg.GoFiles)
		if derr != nil {
			return derr
		}
		if boundary || exemptFuzz {
			hasFuzz, ferr := hasFuncPrefix(p, testFiles, "Fuzz")
			if ferr != nil {
				return ferr
			}
			switch {
			case boundary && !hasFuzz && !exemptFuzz:
				vs = append(vs, Violation{label, "decodes foreign bytes at a network or host boundary but declares no fuzz target: declare a func FuzzXxx(*testing.F)"})
			case exemptFuzz && hasFuzz:
				vs = append(vs, Violation{label, "now has a fuzz target: remove it from the rigor boundary-fuzz exemption list (the list only shrinks)"})
			case exemptFuzz && !boundary:
				vs = append(vs, Violation{label, "no longer decodes at a network or host boundary: remove it from the rigor boundary-fuzz exemption list (the list only shrinks)"})
			}
		}
		if pol.BenchRequired[rel] {
			ok, berr := hasFuncPrefix(p, testFiles, "Benchmark")
			if berr != nil {
				return berr
			}
			if !ok {
				vs = append(vs, Violation{label, "missing a benchmark: declare a func BenchmarkXxx(*testing.B)"})
			}
		}
		return nil
	})
	return vs, err
}

func skipDir(name string) bool {
	switch name {
	case "vendor", "testdata", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func relPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// exempt reports packages not held to the property-test floor: program entry
// points, the version stamp, the *test conformance-helper packages (whose job is
// to test other packages), and this gate itself.
func exempt(rel, pkgName string) bool {
	if pkgName == "main" {
		return true
	}
	switch rel {
	case "internal/version", "internal/rigor":
		return true
	}
	if rel == "cmd" || strings.HasPrefix(rel, "cmd/") || strings.Contains(rel, "/cmd/") {
		return true
	}
	// Conformance-suite helper packages (statetest, spinetest, resourcetest, ...).
	return strings.HasSuffix(path.Base(rel), "test")
}

func importsAny(imports []string, want ...string) bool {
	for _, imp := range imports {
		for _, w := range want {
			if imp == w {
				return true
			}
		}
	}
	return false
}

// hasFuncPrefix reports whether any of the given test files in dir declares a
// top-level single-parameter function whose name has the prefix: "Fuzz" finds a
// fuzz target (func FuzzXxx(*testing.F)), "Benchmark" a benchmark
// (func BenchmarkXxx(*testing.B)).
func hasFuncPrefix(dir string, testFiles []string, prefix string) (bool, error) {
	fset := token.NewFileSet()
	for _, name := range testFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return false, fmt.Errorf("rigor: parse %s: %w", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, prefix) && fn.Type.Params != nil && len(fn.Type.Params.List) == 1 {
				return true, nil
			}
		}
	}
	return false, nil
}

// decodesAtBoundary reports whether the package's production files both reach a
// network or host boundary (boundaryImports) and decode bytes or a reader into Go
// values (decodeFuncs). The two are tested across the package, not per file: the
// file that opens the connection is rarely the file that parses the reply.
//
// It is a conservative signal, not a proof. It cannot see a boundary crossed
// through another package, so a false negative stays possible and the declared
// FuzzRequired list remains the way to demand a fuzz target regardless. What it
// does guarantee is that a new parser wired directly to a socket or a subprocess
// cannot land without a fuzz target, which is how the existing gaps got in.
func decodesAtBoundary(dir string, goFiles []string) (bool, error) {
	fset := token.NewFileSet()
	boundary, decodes := false, false
	for _, name := range goFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return false, fmt.Errorf("rigor: parse %s: %w", name, err)
		}
		decoders := make(map[string]string) // local name -> import path
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if boundaryImports[p] {
				boundary = true
			}
			if _, ok := decodeFuncs[p]; !ok {
				continue
			}
			// A named import rebinds the selector; a blank or dot import cannot
			// produce one to match against.
			local := path.Base(p)
			if imp.Name != nil {
				if imp.Name.Name == "_" || imp.Name.Name == "." {
					continue
				}
				local = imp.Name.Name
			}
			decoders[local] = p
		}
		if decodes || len(decoders) == 0 {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if imp, ok := decoders[id.Name]; ok && decodeFuncs[imp][sel.Sel.Name] {
				decodes = true
				return false
			}
			return true
		})
	}
	return boundary && decodes, nil
}
