package paging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/testkit"
)

// capName matches the names page caps go by (maxPages, maxThreadPages, MaxPage).
var capName = regexp.MustCompile(`(?i)^max\w*pages?$`)

// exempt lists the functions allowed to bound a loop by a page cap themselves, keyed
// by "<path>:<func>", each with the reason paging.All and paging.Prefix do not fit.
var exempt = map[string]string{}

// TestPageLoopsGoThroughPaging fails a production loop bounded by a page cap outside
// this package. Such a loop has to tell "the cap was reached" from "the last page was
// read", and the copies that grew by hand returned the items read so far as the whole
// list: a diff cut at its twentieth page passed as complete and could be approved.
//
// The check is syntactic: it matches a loop whose condition or range expression names
// something called like a page cap. It stops the loop being re-derived by someone who
// does not know this package exists; it does not see a cap under another name.
func TestPageLoopsGoThroughPaging(t *testing.T) {
	root := testkit.RepoRoot(t)
	testkit.EachProductionFunc(t, root, []string{"internal/paging/"}, func(rel string, fn *ast.FuncDecl) {
		key := rel + ":" + fn.Name.Name
		if _, ok := exempt[key]; ok || !capBoundedLoop(fn) {
			return
		}
		t.Errorf("%s bounds a loop by a page cap. Read the pages with paging.All, which fails "+
			"at the cap, or paging.Prefix, which reports whether the list is whole. If neither "+
			"fits, add it to exempt with the reason.", key)
	})
}

// TestExemptionsAreLive fails an exemption that is unreasoned or names a function that
// no longer has a cap-bounded loop, since a stale entry is a hole nobody can see.
func TestExemptionsAreLive(t *testing.T) {
	root := testkit.RepoRoot(t)
	seen := map[string]bool{}
	testkit.EachProductionFunc(t, root, nil, func(rel string, fn *ast.FuncDecl) {
		if capBoundedLoop(fn) {
			seen[rel+":"+fn.Name.Name] = true
		}
	})
	for key, reason := range exempt {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exempt: %q carries no reason", key)
		}
		if !seen[key] {
			t.Errorf("exempt: %q no longer bounds a loop by a page cap; delete it", key)
		}
	}
}

// TestTheGateSeesAHandWrittenPageLoop checks the detector against the loop shapes it
// exists to catch and one it must leave alone, so a gate that matches nothing cannot
// pass for a clean tree.
func TestTheGateSeesAHandWrittenPageLoop(t *testing.T) {
	for src, want := range map[string]bool{
		`func f() { for page := 0; page < maxPages; page++ {} }`:     true,
		`func f() { for range maxThreadPages {} }`:                   true,
		`func f() { for i := 0; i < c.cfg.MaxPages; i++ {} }`:        true,
		`func f() { for i := 0; i < maxRetries; i++ {} }`:            false,
		`func f() { n := maxPages; _ = n; for _, x := range xs {} }`: false,
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\n"+src, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		fn := file.Decls[0].(*ast.FuncDecl)
		if got := capBoundedLoop(fn); got != want {
			t.Errorf("capBoundedLoop(%q) = %v, want %v", src, got, want)
		}
	}
}

// capBoundedLoop reports whether fn has a for loop whose condition or range expression
// names a page cap.
func capBoundedLoop(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		var bound ast.Node
		switch loop := n.(type) {
		case *ast.ForStmt:
			bound = loop.Cond
		case *ast.RangeStmt:
			bound = loop.X
		}
		if bound != nil && namesCap(bound) {
			found = true
		}
		return !found
	})
	return found
}

func namesCap(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && capName.MatchString(id.Name) {
			found = true
		}
		return !found
	})
	return found
}
