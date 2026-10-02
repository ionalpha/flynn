package goal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryStallGoesThroughStatusStall pins the claim on Status.stall: it is the only
// place a goal is written stalled, so Unwired always reflects the reason. Three gates
// once wrote the phase by hand and left a stale Unwired mark behind, which kept a
// breached goal from ever counting as settled. The check is syntactic: it fails any
// production function in this package other than stall that assigns PhaseStalled or
// sets it in a composite literal.
func TestEveryStallGoesThroughStatusStall(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || (fn.Name.Name == "stall" && fn.Recv != nil) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var value ast.Expr
				switch x := n.(type) {
				case *ast.AssignStmt:
					for _, rhs := range x.Rhs {
						if isPhaseStalled(rhs) {
							value = rhs
						}
					}
				case *ast.KeyValueExpr:
					if isPhaseStalled(x.Value) {
						value = x.Value
					}
				}
				if value != nil {
					t.Errorf("%s: %s writes PhaseStalled directly; settle the goal through Status.stall",
						fset.Position(value.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
}

func isPhaseStalled(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "PhaseStalled"
}
