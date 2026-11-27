package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryRunIsGivenAShellPolicy fails a function outside the tests that starts a run
// with [runLearningMission] and never calls [withShellPolicy]. Such a run gets the zero
// policy, which on Windows refuses every command; `skill ab` and `watch` both shipped that
// way, and an A/B measured agents that could not run a single test.
func TestEveryRunIsGivenAShellPolicy(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var callers int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "runLearningMission" {
				continue
			}
			runs, shell := callsIn(fn.Body, "runLearningMission"), callsIn(fn.Body, "withShellPolicy")
			if runs > 0 {
				callers++
				if shell == 0 {
					t.Errorf("%s: %s starts a run without withShellPolicy, so on Windows every command it runs is refused",
						fset.Position(fn.Pos()), fn.Name.Name)
				}
			}
		}
	}
	// The gate reads source; a rename that leaves it matching nothing would pass forever.
	if callers < 3 {
		t.Fatalf("found %d functions that start a run, want at least 3 (run, watch, skill ab): has runLearningMission been renamed?", callers)
	}
}

// callsIn counts the calls to the package-level function name inside n.
func callsIn(n ast.Node, name string) int {
	var count int
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
				count++
			}
		}
		return true
	})
	return count
}
