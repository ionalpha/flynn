package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// extendOwnerDir is the package that declares Ledger.Extend; calls inside it are the
// method's own implementation.
const extendOwnerDir = "budget"

// extendCallerAllowList names the non-test files that may call Ledger.Extend, each with
// the reason. Raising a run's budget ceiling spends money nobody agreed to when the
// model or a tool can reach it, so the only legitimate caller is the entry point that a
// person's request arrives at in the conversation core, and it is added here in the same
// change that writes it. The test fails on an entry whose file no longer calls Extend, so
// the list only holds what is real.
var extendCallerAllowList = map[string]string{}

// extendCall is one place a file selects a method named Extend.
type extendCall struct{ where string }

// extendCalls returns every selector named Extend in src: a call, a method value and a
// method expression all count, because each reaches the same method. The match is on the
// name and not on a resolved type, so a caller that obtained the ledger from a function
// without importing the budget package is still found; a different type with an Extend
// method would need an allow-list entry with its reason, which is the cheaper mistake.
func extendCalls(t *testing.T, filename string, src any) []extendCall {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var out []extendCall
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Extend" {
			out = append(out, extendCall{where: fset.Position(sel.Sel.Pos()).String()})
		}
		return true
	})
	return out
}

// TestOnlyTheHumanEntryPointCallsLedgerExtend holds the threat model's rule that a model
// or tool cannot raise its own budget: nothing in the module outside the budget package
// calls Ledger.Extend unless it is on the allow-list above. The runtime check in Extend
// (a context bound to a run budget is refused) is the second lock; this one fails the
// build when someone adds a caller, so the review question is asked at the line that
// creates the path and not after.
func TestOnlyTheHumanEntryPointCallsLedgerExtend(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	found := map[string]bool{}
	var bad []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if path.Dir(rel) == extendOwnerDir {
			return nil
		}
		calls := extendCalls(t, p, nil)
		if len(calls) == 0 {
			return nil
		}
		if _, ok := extendCallerAllowList[rel]; ok {
			found[rel] = true
			return nil
		}
		for _, c := range calls {
			bad = append(bad, c.where)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s calls Extend: only the human-authorized entry point in the conversation core may raise a run's budget ceiling; add that file to extendCallerAllowList with the reason", b)
	}
	for file, reason := range extendCallerAllowList {
		if reason == "" {
			t.Errorf("allow-list entry %q has no reason", file)
		}
		if !found[file] {
			t.Errorf("allow-list entry %q no longer calls Extend: remove it", file)
		}
	}
}

// TestExtendCallDetectorSeesEveryWayToReachTheMethod keeps the guard above honest: a
// call through a field, a method value and a method expression are found, the same name
// as a plain function is not, and a caller that never names the budget package is found
// too.
func TestExtendCallDetectorSeesEveryWayToReachTheMethod(t *testing.T) {
	src := `package p
import "github.com/ionalpha/flynn/budget"
func a(h holder) { _, _ = h.ledger.Extend(nil, "r", budget.Extension{}) }
func b(l *budget.Ledger) { f := l.Extend; _ = f }
func c() { _ = (*budget.Ledger).Extend }
func d(h holder) { _ = h.Budget().Extend }
func e() { Extend() }
func f(l *budget.Ledger) { _ = l.WithExtensionCap(budget.Limits{}) }
`
	got := extendCalls(t, "x.go", src)
	if len(got) != 4 {
		t.Fatalf("detector found %d selectors, want 4 (a, b, c, d): %v", len(got), got)
	}
}
