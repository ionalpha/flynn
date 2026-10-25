package rigor_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// longBlockLines is the length at which a declaration comment stops being an
// invariant and starts being prose. A block this long is usually restating the
// code, telling its history, or holding an argument that belongs in a design
// document; AGENTS.md asks for the invariant, the failure it guards against, or
// the ordering reason, said once.
const longBlockLines = 15

// longBlockAllowlist names the declaration comments of longBlockLines or more
// that must stay, each with the reason. A key is "<file>: <declaration>". The
// test fails on an entry whose comment has fallen below the threshold, so the
// list only shrinks.
var longBlockAllowlist = map[string]string{
	"sandbox/restricted_windows.go: createWriteRestrictedToken": "records the failure codes found by testing on Windows; the codes cannot be re-derived from the code",
}

// longBlockCeiling is the number of blocks at or over longBlockLines that are
// not in longBlockAllowlist. It is debt: lower it when a block is cut, never
// raise it. A new long comment must be cut or allowlisted with a reason.
const longBlockCeiling = 0

// TestDeclarationCommentsStayShort fails when the count of declaration comments
// of longBlockLines or more rises above the ceiling, and when an allowlisted
// block no longer needs its entry. It reads every non-test Go file of the module
// and counts the doc comment of each declaration, spec and struct field, so a
// long comment cannot move from a function to the type beside it unseen.
func TestDeclarationCommentsStayShort(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	blocks := longBlocks(t, root)

	var over []string
	seen := map[string]bool{}
	for _, b := range blocks {
		seen[b.key] = true
		if _, ok := longBlockAllowlist[b.key]; !ok {
			over = append(over, fmt.Sprintf("%s (%d lines)", b.where, b.lines))
		}
	}
	for key, reason := range longBlockAllowlist {
		if reason == "" {
			t.Errorf("allowlist entry %q has no reason", key)
		}
		if !seen[key] {
			t.Errorf("allowlist entry %q is no longer a %d+ line comment: remove it", key, longBlockLines)
		}
	}

	slices.Sort(over)
	switch {
	case len(over) > longBlockCeiling:
		t.Errorf("%d declaration comments of %d+ lines, ceiling %d. Cut each to the invariant, the failure it guards against, or the ordering reason, said once on the declaration:\n  %s",
			len(over), longBlockLines, longBlockCeiling, strings.Join(over, "\n  "))
	case len(over) < longBlockCeiling:
		t.Errorf("%d declaration comments of %d+ lines, below ceiling %d: lower longBlockCeiling to %d so the gain cannot be spent again",
			len(over), longBlockLines, longBlockCeiling, len(over))
	}
}

// TestLongBlockFinderCountsDocComments pins the finder: a doc comment on a
// function, a type, a spec in a group and a struct field is counted, a short
// one and the package comment are not.
func TestLongBlockFinderCountsDocComments(t *testing.T) {
	long := strings.Repeat("// line\n", longBlockLines)
	short := strings.Repeat("// line\n", longBlockLines-1)
	src := long + "package p\n\n" +
		long + "func F() {}\n\n" +
		short + "func Short() {}\n\n" +
		"type T struct {\n" + long + "\tField int\n}\n\n" +
		"var (\n" + long + "\tV = 1\n)\n"

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range blocksIn(fset, f, "p.go") {
		got = append(got, b.key)
	}
	slices.Sort(got)
	want := []string{"p.go: F", "p.go: T.Field", "p.go: V"}
	if !slices.Equal(got, want) {
		t.Fatalf("blocks = %v, want %v", got, want)
	}
}

// longBlock is one doc comment at or over longBlockLines.
type longBlock struct {
	key   string // "<file>: <declaration>", the allowlist key
	where string // "<file>:<line>: <declaration>", for the failure message
	lines int
}

// longBlocks returns every long declaration comment in the non-test Go files
// under root, skipping the directories the rest of the gate skips.
func longBlocks(t *testing.T, root string) []longBlock {
	t.Helper()
	var out []longBlock
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "node_modules" ||
				strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		out = append(out, blocksIn(fset, f, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// blocksIn returns the long doc comments of one parsed file. The package
// comment is out of scope: it is the package's reference page.
func blocksIn(fset *token.FileSet, f *ast.File, rel string) []longBlock {
	var out []longBlock
	add := func(doc *ast.CommentGroup, name string) {
		if doc == nil {
			return
		}
		start, end := fset.Position(doc.Pos()), fset.Position(doc.End())
		n := end.Line - start.Line + 1
		if n < longBlockLines {
			return
		}
		out = append(out, longBlock{
			key:   rel + ": " + name,
			where: fmt.Sprintf("%s:%d: %s", rel, start.Line, name),
			lines: n,
		})
	}

	var owner string // enclosing type name, to name its fields
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			name := n.Name.Name
			if n.Recv != nil && len(n.Recv.List) == 1 {
				name = typeName(n.Recv.List[0].Type) + "." + name
			}
			add(n.Doc, name)
		case *ast.GenDecl:
			// A lone spec's doc comment hangs on the declaration; the specs of
			// a group carry their own, visited below.
			add(n.Doc, specName(n.Specs))
		case *ast.TypeSpec:
			add(n.Doc, n.Name.Name)
			owner = n.Name.Name
		case *ast.ValueSpec:
			add(n.Doc, n.Names[0].Name)
		case *ast.Field:
			if len(n.Names) > 0 {
				add(n.Doc, owner+"."+n.Names[0].Name)
			} else {
				add(n.Doc, owner+"."+typeName(n.Type))
			}
		}
		return true
	})
	return out
}

// typeName is the identifier a receiver or embedded field names, without the
// pointer or type arguments.
func typeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.IndexExpr:
		return typeName(e.X)
	case *ast.IndexListExpr:
		return typeName(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return "?"
}

// specName names a declaration by its first spec.
func specName(specs []ast.Spec) string {
	if len(specs) == 0 {
		return "?"
	}
	switch s := specs[0].(type) {
	case *ast.TypeSpec:
		return s.Name.Name
	case *ast.ValueSpec:
		return s.Names[0].Name
	case *ast.ImportSpec:
		return "import"
	}
	return "?"
}
