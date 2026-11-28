package verdict_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/testkit"
)

// outcomes are the names of the three answers, as a switch over them spells them.
var outcomes = map[string]bool{"Unknown": true, "Passed": true, "Failed": true}

// TestOutcomeSwitchesHaveNoDefault fails a production switch over a verdict's outcome
// that has a default clause. The exhaustive linter counts a default as covering every
// value, so with one present a switch that names only Passed and Failed passes lint and
// sends Unknown to whatever the default does, which is how a check that could not run
// comes to count as one that failed, or one that passed.
//
// The check is syntactic: it matches a switch on a call to a method named Outcome, or
// one whose cases name verdict.Unknown, verdict.Passed or verdict.Failed.
func TestOutcomeSwitchesHaveNoDefault(t *testing.T) {
	root := testkit.RepoRoot(t)
	testkit.EachProductionFunc(t, root, nil, func(rel string, fn *ast.FuncDecl) {
		own := strings.HasPrefix(rel, "internal/verdict/")
		if defaultedOutcomeSwitch(fn, own) {
			t.Errorf("%s:%s switches over a verdict.Outcome with a default clause. Name every "+
				"outcome, Unknown included, and drop the default.", rel, fn.Name.Name)
		}
	})
}

// TestTheGateSeesADefaultedSwitch checks the detector against the shapes it exists to
// catch and the ones it must leave alone.
func TestTheGateSeesADefaultedSwitch(t *testing.T) {
	for src, want := range map[string]bool{
		`func f() { switch v.Outcome() { case verdict.Passed: ; default: } }`:                              true,
		`func f() { switch o { case verdict.Failed: ; default: } }`:                                        true,
		`func f() { switch v.Outcome() { case verdict.Passed, verdict.Failed: ; case verdict.Unknown: } }`: false,
		`func f() { switch s { case "a": ; default: } }`:                                                   false,
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\n"+src, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if got := defaultedOutcomeSwitch(file.Decls[0].(*ast.FuncDecl), false); got != want {
			t.Errorf("defaultedOutcomeSwitch(%q) = %v, want %v", src, got, want)
		}
	}
}

// defaultedOutcomeSwitch reports whether fn has a switch over an outcome with a default
// clause. own is true inside this package, where the outcomes are bare identifiers.
func defaultedOutcomeSwitch(fn *ast.FuncDecl, own bool) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || found {
			return !found
		}
		overOutcome := isOutcomeCall(sw.Tag)
		hasDefault := false
		for _, stmt := range sw.Body.List {
			clause := stmt.(*ast.CaseClause)
			if clause.List == nil {
				hasDefault = true
			}
			for _, e := range clause.List {
				overOutcome = overOutcome || namesOutcome(e, own)
			}
		}
		found = overOutcome && hasDefault
		return !found
	})
	return found
}

func isOutcomeCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Outcome"
}

func namesOutcome(e ast.Expr, own bool) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && pkg.Name == "verdict" && outcomes[x.Sel.Name]
	case *ast.Ident:
		return own && outcomes[x.Name]
	}
	return false
}
