package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// tokenFields are the names of a model call's token counters, on llm.Usage and on the
// session.Usage that mirrors it.
var tokenFields = map[string]bool{
	"InputTokens": true, "OutputTokens": true, "CacheReadTokens": true, "CacheWriteTokens": true,
}

// usageSumDirs are the directories allowed to add token counters by hand. The adapters
// translate a provider's own accounting into llm.Usage, where adding is the translation
// (Anthropic reports cache reads outside its input count), and package usage owns the
// definition everyone else asks.
var usageSumDirs = []string{"llm/anthropic/", "llm/openai/", "usage/"}

// usageSumExemptions names the other non-test files that add token counters, each with the
// reason. The test fails on an entry whose file no longer does, so the list only shrinks.
var usageSumExemptions = map[string]string{}

// tokenSums returns the lines of src that add a token counter: a + between operands one
// of which reads a counter field, or a += whose target or value does.
func tokenSums(t *testing.T, filename string, src any) []int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	isCounter := func(e ast.Expr) bool {
		for {
			p, ok := e.(*ast.ParenExpr)
			if !ok {
				break
			}
			e = p.X
		}
		sel, ok := e.(*ast.SelectorExpr)
		return ok && tokenFields[sel.Sel.Name]
	}
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op == token.ADD && (isCounter(n.X) || isCounter(n.Y)) {
				lines = append(lines, fset.Position(n.OpPos).Line)
			}
		case *ast.AssignStmt:
			if n.Tok == token.ADD_ASSIGN && (isCounter(n.Lhs[0]) || isCounter(n.Rhs[0])) {
				lines = append(lines, fset.Position(n.TokPos).Line)
			}
		}
		return true
	})
	return lines
}

// TestTokenCountersAreSummedOnlyByPackageUsage fails when production code adds a model
// call's token counters itself. Hand-written sums disagreed about what a total is (one
// counted cache reads twice, others left out cache writes), so a total is
// [usage.Tokens.Total] and an accumulation is [usage.Tokens.Add].
func TestTokenCountersAreSummedOnlyByPackageUsage(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	found := map[string]bool{}
	var bad []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, dir := range usageSumDirs {
			if strings.HasPrefix(rel, dir) {
				return nil
			}
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from walking the repository
		if err != nil {
			return err
		}
		lines := tokenSums(t, path, data)
		if len(lines) == 0 {
			return nil
		}
		if _, ok := usageSumExemptions[rel]; ok {
			found[rel] = true
			return nil
		}
		for _, l := range lines {
			bad = append(bad, rel+":"+strconv.Itoa(l))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s: token counters are added by hand; use usage.FromLLM (or session.Usage.Tokens) with usage.Tokens.Add and usage.Tokens.Total", b)
	}
	for file, reason := range usageSumExemptions {
		if reason == "" {
			t.Errorf("exemption %q has no reason", file)
		}
		if !found[file] {
			t.Errorf("exemption %q no longer adds token counters: remove it", file)
		}
	}
}

// TestTokenSumDetectorFlagsTheBadShapes keeps the gate above honest: every way of adding
// counters by hand is found, and arithmetic that is not a sum of counters is not.
func TestTokenSumDetectorFlagsTheBadShapes(t *testing.T) {
	src := `package p
func plus(u U) int { return u.InputTokens + u.CacheWriteTokens }
func plusConst(u U) int { return u.OutputTokens + 1 }
func parens(u U) int { return 2 + (u.CacheReadTokens) }
func accumulate(p *P, u U) { p.Usage.InputTokens += u.InputTokens }
func fromLocal(p *P, n int) { p.OutputTokens += n }
func intoLocal(u U) (n int) { n += u.CacheWriteTokens; return }
func minus(u U) int { return u.InputTokens - u.CacheReadTokens }
func scale(u U) int { return u.InputTokens * 2 }
func other(u U) int { return u.Tokens + u.Count }
func plain(a, b int) int { return a + b }
func read(u U) int { return u.InputTokens }
`
	got := map[int]bool{}
	for _, l := range tokenSums(t, "x.go", src) {
		got[l] = true
	}
	for i, l := range strings.Split(src, "\n") {
		want := strings.HasPrefix(l, "func plus") || strings.HasPrefix(l, "func parens") ||
			strings.HasPrefix(l, "func accumulate") || strings.HasPrefix(l, "func fromLocal") ||
			strings.HasPrefix(l, "func intoLocal")
		if got[i+1] != want {
			t.Errorf("line %d %.30q: flagged=%v, want %v", i+1, l, got[i+1], want)
		}
	}
}
