package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// composition lists the constructors that assemble a governed run. They are called by
// package governed and by nothing in cmd/flynn: a binary that builds its own runtime,
// executor or judges has a second definition of what "governed" means, which is the
// divergence the exported assembly exists to remove. The binary reaches them only through
// governed.Assemble, governed.NewKit and the other exported entry points.
var composition = map[string][]string{
	"github.com/ionalpha/flynn/runtime":               {"New"},
	"github.com/ionalpha/flynn/mission":               {"NewExecutor", "NewPlanner", "WithGrant", "WithSandbox", "WithBrakes", "WithBudget", "WithEventSink"},
	"github.com/ionalpha/flynn/driver":                {"NewRouter"},
	"github.com/ionalpha/flynn/evidence":              {"NewCommandAuditor", "NewCommandVerifier", "NewModelAuditor", "NewModelSteerJudge", "NewSpineEvidence"},
	"github.com/ionalpha/flynn/orchestration":         {"NewSpawner", "Units", "UnitGovernor"},
	"github.com/ionalpha/flynn/capability":            {"NewContainmentGate"},
	"github.com/ionalpha/flynn/extension":             {"NewRegistry", "NewLoader", "NewProcessHandler"},
	"github.com/ionalpha/flynn/progress":              {"NewSpineProbe"},
	"github.com/ionalpha/flynn/memory/curate":         {"Wrap"},
	"github.com/ionalpha/flynn/memory/digest":         {"New"},
	"github.com/ionalpha/flynn/memory/ridealong":      {"New"},
	"github.com/ionalpha/flynn/memory/hybrid":         {"Wrap"},
	"github.com/ionalpha/flynn/learn":                 {"NewGovernedVerifier", "NewGovernedDistiller"},
	"github.com/ionalpha/flynn/internal/integrations": {"NewHandler"},
	"github.com/ionalpha/flynn/internal/ops":          {"RegisterWith"},
	"github.com/ionalpha/flynn/refusal":               {"NewSpineProbe"},
	"github.com/ionalpha/flynn/approval":              {"NewGate"},
	"github.com/ionalpha/flynn/allowance":             {"NewGate"},
}

// compositionCalls returns "pkg.Name" for every reference in src to a constructor in
// composition, resolved through the file's own import names.
func compositionCalls(t *testing.T, filename string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	names := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		local := filepath.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		names[local] = p
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		path, ok := names[id.Name]
		if !ok {
			return true
		}
		for _, banned := range composition[path] {
			if sel.Sel.Name == banned {
				out = append(out, filepath.Base(path)+"."+banned+" at "+fset.Position(sel.Pos()).String())
			}
		}
		return true
	})
	return out
}

// TestBinaryDoesNotComposeAGovernedRun fails when cmd/flynn calls a constructor that
// assembles part of a governed run. A guarantee that every run the binary starts is
// composed by package governed needs a test that breaks when a call creeps back in; this
// is it.
func TestBinaryDoesNotComposeAGovernedRun(t *testing.T) {
	dir := filepath.Join("..", "..", "cmd", "flynn")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		found = append(found, compositionCalls(t, filepath.Join(dir, name), nil)...)
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("cmd/flynn composes a governed run itself (%s); move it into package governed", f)
	}
}

// TestCompositionDetectorSeesAliasedAndDirectCalls keeps the guard above honest: a rename
// of an import must not hide a call from it.
func TestCompositionDetectorSeesAliasedAndDirectCalls(t *testing.T) {
	src := `package main
import (
	rt "github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/evidence"
	"github.com/ionalpha/flynn/governed"
)
func f() { _, _ = rt.New(rt.Config{}); _ = evidence.NewCommandAuditor; _, _ = governed.Assemble }
`
	got := compositionCalls(t, "x.go", src)
	if len(got) != 2 {
		t.Fatalf("detector found %v, want runtime.New and evidence.NewCommandAuditor", got)
	}
}
