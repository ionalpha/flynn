// Package rigor enforces the project's engineering-rigor floor as a test rather
// than a hope: every production package must carry a property test (rapid or the
// testkit harness), and a declared set must carry a fuzz target. Because the gate
// is an ordinary Go test (see rigor_test.go), it runs inside dev/test, dev/check,
// and the CI test matrix with no extra wiring, so a package added without its
// required tests turns `go test ./...` red locally and in CI.
//
// New packages are held to the floor immediately. A grandfather allowlist covers
// the gaps that predate the gate so it lands green; the list only ever shrinks
// (the gate fails if a grandfathered package starts complying, forcing its
// removal), and nothing new is added to it.
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
}

// DefaultPolicy is the policy the live gate enforces. The empty string is the
// root (module) package.
func DefaultPolicy() Policy {
	return Policy{
		Grandfathered: map[string]bool{
			"":                 true, // root agent package
			"clock":            true,
			"dispatch":         true,
			"ids":              true,
			"internal/sqlitex": true,
			"observe":          true,
			"spinesink":        true,
		},
		FuzzRequired: map[string]bool{
			"bus":      true,
			"fault":    true,
			"spine":    true,
			"resource": true,
		},
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

		if pol.FuzzRequired[rel] {
			ok, ferr := hasFuzzTarget(p, append(append([]string{}, pkg.TestGoFiles...), pkg.XTestGoFiles...))
			if ferr != nil {
				return ferr
			}
			if !ok {
				vs = append(vs, Violation{label, "missing a fuzz target: declare a func FuzzXxx(*testing.F)"})
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

// hasFuzzTarget reports whether any of the given test files in dir declares a
// top-level fuzz target: func FuzzXxx(f *testing.F).
func hasFuzzTarget(dir string, testFiles []string) (bool, error) {
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
			if strings.HasPrefix(fn.Name.Name, "Fuzz") && fn.Type.Params != nil && len(fn.Type.Params.List) == 1 {
				return true, nil
			}
		}
	}
	return false, nil
}
