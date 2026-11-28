package rigor_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"
)

// deterministicPackages are the packages whose output must be the same on every
// run and every backend, each with the packages below it. The sort.Slice rule in
// dev/rules.go covers the same list.
var deterministicPackages = []string{
	"chain", "goal", "hlc", "internal/migrate", "internal/spinesink", "internal/sqlitex", "jobs",
	"mission", "resource", "session", "spine", "state", "storage/sqlite", "usage",
}

// orderedDirective on a loop's line, or the line above, says why the order the
// loop visits a map in cannot be observed.
const orderedDirective = "//ordered:ok"

// mapOrder reports a range over a map whose body can pass the visiting order on.
// It accepts a body only when every statement in it gives the same result in any
// order, so a statement it does not recognise is reported.
var mapOrder = &analysis.Analyzer{
	Name: "maporder",
	Doc:  "reports a range over a map that can feed ordered output",
	Run:  runMapOrder,
}

// harnessPackage matches a conformance or double package, such as statetest,
// which tests import and nothing ships.
var harnessPackage = regexp.MustCompile(`[a-z]test$`)

func runMapOrder(pass *analysis.Pass) (any, error) {
	if harnessPackage.MatchString(pass.Pkg.Path()) {
		return nil, nil
	}
	for _, file := range pass.Files {
		if strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		marks := map[int]*ast.Comment{}
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, orderedDirective) {
					marks[pass.Fset.Position(c.Pos()).Line] = c
				}
			}
		}
		used := map[*ast.Comment]bool{}
		for _, decl := range file.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				loop, ok := n.(*ast.RangeStmt)
				if !ok || !rangesOverMap(pass.TypesInfo, loop.X) {
					return true
				}
				check := &loopCheck{info: pass.TypesInfo, decl: decl, loop: loop}
				bad := check.stmt(loop.Body, 0)
				if bad == nil {
					return true
				}
				line := pass.Fset.Position(loop.Pos()).Line
				mark := marks[line]
				if mark == nil {
					mark = marks[line-1]
				}
				if mark == nil {
					pass.Reportf(loop.Pos(), "range over a map can pass its random order on (line %d): range over slices.Sorted(maps.Keys(m)), or say why order cannot be observed with %s <reason>",
						pass.Fset.Position(bad.Pos()).Line, orderedDirective)
					return true
				}
				used[mark] = true
				reason, _, _ := strings.Cut(strings.TrimPrefix(mark.Text, orderedDirective), "//")
				if strings.TrimSpace(reason) == "" {
					pass.Reportf(loop.Pos(), "%s needs a reason", orderedDirective)
				}
				return true
			})
		}
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, orderedDirective) && !used[c] {
					pass.Reportf(c.Pos(), "%s marks no loop that needs it: remove it", orderedDirective)
				}
			}
		}
	}
	return nil, nil
}

// rangesOverMap reports whether x is a map, or one of the iterators of package
// maps, which visit in the same random order.
func rangesOverMap(info *types.Info, x ast.Expr) bool {
	if call, ok := ast.Unparen(x).(*ast.CallExpr); ok {
		switch calleeName(info, call) {
		case "maps.Keys", "maps.Values", "maps.All":
			return true
		}
	}
	t := info.TypeOf(x)
	if t == nil {
		return false
	}
	if p, ok := t.(*types.TypeParam); ok {
		// A type parameter ranges as its constraint's first term does.
		iface, ok := p.Constraint().Underlying().(*types.Interface)
		if !ok || iface.NumEmbeddeds() != 1 {
			return false
		}
		t = iface.EmbeddedType(0)
		if u, ok := t.(*types.Union); ok {
			t = u.Term(0).Type()
		}
	}
	_, ok := t.Underlying().(*types.Map)
	return ok
}

// calleeName is "pkg.Func" for a package function, "name" for a builtin, else "".
func calleeName(info *types.Info, call *ast.CallExpr) string {
	fun := ast.Unparen(call.Fun)
	if index, ok := fun.(*ast.IndexExpr); ok {
		fun = index.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		if b, ok := info.Uses[f].(*types.Builtin); ok {
			return b.Name()
		}
	case *ast.SelectorExpr:
		fn, ok := info.Uses[f.Sel].(*types.Func)
		if ok && fn.Pkg() != nil && fn.Signature().Recv() == nil {
			return fn.Pkg().Path() + "." + fn.Name()
		}
	}
	return ""
}

// loopCheck judges the statements of one map loop.
type loopCheck struct {
	info *types.Info
	decl ast.Decl
	loop *ast.RangeStmt
}

// stmt returns the first statement under s whose effect depends on visiting
// order, or nil. depth counts the loops and switches between s and the map loop,
// which an unlabelled break would leave instead of it.
func (c *loopCheck) stmt(s ast.Stmt, depth int) ast.Stmt {
	switch s := s.(type) {
	case nil, *ast.EmptyStmt, *ast.DeclStmt, *ast.IncDecStmt:
		return nil
	case *ast.BlockStmt:
		return c.first(depth, s.List...)
	case *ast.CaseClause:
		return c.first(depth, s.Body...)
	case *ast.IfStmt:
		if c.isExtreme(s) {
			return nil
		}
		return c.first(depth, s.Init, s.Body, s.Else)
	case *ast.SwitchStmt:
		return c.first(depth+1, s.Init, s.Body)
	case *ast.TypeSwitchStmt:
		return c.first(depth+1, s.Init, s.Body)
	case *ast.ForStmt:
		return c.first(depth+1, s.Init, s.Post, s.Body)
	case *ast.RangeStmt:
		return c.first(depth+1, s.Body)
	case *ast.BranchStmt:
		if s.Label == nil && (s.Tok != token.BREAK || depth > 0) && s.Tok != token.GOTO {
			return nil
		}
	case *ast.AssignStmt:
		if c.assignOK(s) {
			return nil
		}
	case *ast.ExprStmt:
		if c.callOK(s.X) {
			return nil
		}
	}
	return s
}

func (c *loopCheck) first(depth int, list ...ast.Stmt) ast.Stmt {
	for _, s := range list {
		if bad := c.stmt(s, depth); bad != nil {
			return bad
		}
	}
	return nil
}

// isExtreme recognises "if a > b { b = a }": a maximum or minimum of a basic
// type is the same in any order.
func (c *loopCheck) isExtreme(s *ast.IfStmt) bool {
	cond, ok := s.Cond.(*ast.BinaryExpr)
	if !ok || s.Else != nil || len(s.Body.List) != 1 {
		return false
	}
	switch cond.Op {
	case token.LSS, token.LEQ, token.GTR, token.GEQ:
	default:
		return false
	}
	assign, ok := s.Body.List[0].(*ast.AssignStmt)
	if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	t := c.info.TypeOf(assign.Lhs[0])
	if t == nil {
		return false
	}
	if _, basic := t.Underlying().(*types.Basic); !basic {
		return false
	}
	to, from := types.ExprString(assign.Lhs[0]), types.ExprString(assign.Rhs[0])
	x, y := types.ExprString(cond.X), types.ExprString(cond.Y)
	return (to == x && from == y) || (to == y && from == x)
}

func (c *loopCheck) assignOK(s *ast.AssignStmt) bool {
	switch s.Tok {
	case token.DEFINE:
		return true
	case token.ASSIGN:
		for i, lhs := range s.Lhs {
			var rhs ast.Expr
			if len(s.Rhs) == len(s.Lhs) {
				rhs = s.Rhs[i]
			}
			if !c.storeOK(lhs, rhs) {
				return false
			}
		}
		return true
	}
	// A compound assignment: integer arithmetic and bit operations commute;
	// string joining and float addition do not give the same result reordered.
	lhs := s.Lhs[0]
	if c.isLocal(lhs) || c.indexedByKey(lhs) {
		return true
	}
	t := c.info.TypeOf(lhs)
	if t == nil {
		return false
	}
	basic, ok := t.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsInteger == 0 {
		return false
	}
	switch s.Tok {
	case token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.OR_ASSIGN, token.AND_ASSIGN, token.XOR_ASSIGN, token.AND_NOT_ASSIGN:
		return true
	}
	return false
}

// storeOK judges "lhs = rhs".
func (c *loopCheck) storeOK(lhs, rhs ast.Expr) bool {
	if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
		return true
	}
	if c.isLocal(lhs) {
		return true
	}
	if call, ok := ast.Unparen(rhs).(*ast.CallExpr); ok && calleeName(c.info, call) == "append" {
		// Appending keeps the visiting order, unless each element of the map
		// has its own bucket or the whole slice is sorted afterwards.
		return c.indexedByKey(lhs) || c.sortedAfter(lhs)
	}
	if index, ok := lhs.(*ast.IndexExpr); ok {
		if t := c.info.TypeOf(index.X); t != nil {
			if _, isMap := t.Underlying().(*types.Map); isMap {
				return true
			}
		}
	}
	if rhs == nil {
		return false
	}
	tv := c.info.Types[rhs]
	return tv.Value != nil || tv.IsNil()
}

// isLocal reports whether e is, or is reached through, a variable declared by
// the loop or inside it.
func (c *loopCheck) isLocal(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.Ident:
			obj := c.info.ObjectOf(x)
			return obj != nil && obj.Pos() >= c.loop.Pos() && obj.Pos() < c.loop.End()
		default:
			return false
		}
	}
}

// indexedByKey reports whether e is an entry selected by the loop's own key.
func (c *loopCheck) indexedByKey(e ast.Expr) bool {
	index, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	key, ok := c.loop.Key.(*ast.Ident)
	if !ok {
		return false
	}
	used, ok := index.Index.(*ast.Ident)
	return ok && c.info.ObjectOf(key) == c.info.ObjectOf(used)
}

// callOK accepts delete, and sorting something that belongs to this element.
func (c *loopCheck) callOK(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	name := calleeName(c.info, call)
	if name == "delete" {
		return true
	}
	if name == "copy" {
		return c.isLocal(call.Args[0])
	}
	return (strings.HasPrefix(name, "sort.") || strings.HasPrefix(name, "slices.Sort")) &&
		len(call.Args) > 0 && c.isLocal(call.Args[0])
}

// sortedAfter reports whether the function sorts e after the loop with a sort
// that has one result for any input order. A sort with a comparison function is
// not accepted: nothing here can tell whether it breaks every tie.
func (c *loopCheck) sortedAfter(e ast.Expr) bool {
	want := types.ExprString(e)
	found := false
	ast.Inspect(c.decl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || call.Pos() < c.loop.End() || len(call.Args) != 1 {
			return true
		}
		switch calleeName(c.info, call) {
		case "slices.Sort", "sort.Strings", "sort.Ints":
			found = found || types.ExprString(call.Args[0]) == want
		}
		return true
	})
	return found
}

// TestMapOrderDoesNotReachOutput fails when a deterministic package ranges over
// a map in a way that can pass the order on.
func TestMapOrderDoesNotReachOutput(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}
	var patterns []string
	for _, p := range deterministicPackages {
		patterns = append(patterns, "./"+p+"/...")
	}
	pkgs, err := packages.Load(&packages.Config{Mode: packages.LoadSyntax | packages.NeedModule, Dir: root}, patterns...)
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	loaded := map[string]bool{}
	for _, p := range pkgs {
		for _, e := range p.Errors {
			t.Fatalf("%s: %v", p.PkgPath, e)
		}
		loaded[p.PkgPath] = true
	}
	for _, p := range deterministicPackages {
		if !loaded["github.com/ionalpha/flynn/"+p] {
			t.Errorf("listed package %s was not loaded", p)
		}
	}
	graph, err := checker.Analyze([]*analysis.Analyzer{mapOrder}, pkgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, act := range graph.Roots {
		if act.Err != nil {
			t.Fatalf("%s: %v", act.Package.PkgPath, act.Err)
		}
		for _, d := range act.Diagnostics {
			found = append(found, act.Package.Fset.Position(d.Pos).String()+": "+d.Message)
		}
	}
	slices.Sort(found)
	for _, f := range found {
		t.Error(f)
	}
}

// TestMapOrderExamples holds the check to its bad and good examples.
func TestMapOrderExamples(t *testing.T) {
	testdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	analysistest.Run(t, testdata, mapOrder, "maporder")
}

// TestSortRuleCoversTheSamePackages keeps dev/rules.go and this file agreeing on
// which packages are deterministic.
func TestSortRuleCoversTheSamePackages(t *testing.T) {
	rules, err := os.ReadFile(filepath.Join("..", "..", "dev", "rules.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := "(" + strings.Join(deterministicPackages, "|") + ")(/|$)"
	if !strings.Contains(string(rules), want) {
		t.Errorf("dev/rules.go does not limit sort.Slice to %s", want)
	}
}
