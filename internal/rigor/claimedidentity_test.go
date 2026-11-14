package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// claimedFields are the fields that hold what an attached client said about itself. They
// are recorded for a person reading the history and for nothing else: the client chose
// the words, so a branch on them would let any peer pick the branch.
var claimedFields = map[string]bool{"ClaimedName": true, "ClaimedVersion": true}

// claimedHome is the one file allowed to touch the fields, and the one function in it:
// the step that makes the text inert before it is written. The fields are declared in the
// same file, which is not a use.
const (
	claimedHome   = "session/headless.go"
	claimedHandle = "sanitized"
)

// claimedUse is one place a file names a claimed-identity field.
type claimedUse struct {
	where string
	in    string // the enclosing function, empty at package level
}

// claimedUses returns every selector or composite-literal key in src that names a claimed
// field, with the function it sits in. The match is on the field name alone, so it needs
// no type information and cannot be sidestepped by passing the value through a variable.
func claimedUses(t *testing.T, filename string, src any) []claimedUse {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var out []claimedUse
	for _, decl := range f.Decls {
		fn := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if claimedFields[x.Sel.Name] {
					out = append(out, claimedUse{fset.Position(x.Sel.Pos()).String(), fn})
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && claimedFields[id.Name] {
					out = append(out, claimedUse{fset.Position(id.Pos()).String(), fn})
				}
			}
			return true
		})
	}
	return out
}

// TestClaimedIdentityIsNeverRead fails when production code reads the name or version an
// attached client gave itself. Only the step that makes the text inert may touch them, so
// no authorization, routing or trust decision can depend on a string the peer wrote.
func TestClaimedIdentityIsNeverRead(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	var bad []string
	sawHome := false
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
		for _, u := range claimedUses(t, path, nil) {
			if rel == claimedHome && u.in == claimedHandle {
				sawHome = true
				continue
			}
			bad = append(bad, u.where+" in "+u.in)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s uses a client's claimed name or version: it is recorded as unattested text and must not decide anything", b)
	}
	if !sawHome {
		t.Errorf("%s no longer touches the claimed fields in %s: update this test with the code", claimedHome, claimedHandle)
	}
}

// TestClaimedIdentityDetectorSeesReadsAndLiterals keeps the guard above honest: a read
// through a variable, a comparison and a composite-literal key are all found, and an
// unrelated field is not.
func TestClaimedIdentityDetectorSeesReadsAndLiterals(t *testing.T) {
	src := `package p
type A struct{ ClaimedName, ClaimedVersion, Name string }
func a(c A) bool { n := c.ClaimedName; return n == "trusted" }
func b(c *A) string { return c.ClaimedVersion }
func c() A { return A{ClaimedName: "x"} }
func d(c A) string { return c.Name }
`
	got := claimedUses(t, "x.go", src)
	var fns []string
	for _, u := range got {
		fns = append(fns, u.in)
	}
	sort.Strings(fns)
	if want := "a,b,c"; strings.Join(fns, ",") != want {
		t.Fatalf("detector found uses in %q, want %q", fns, want)
	}
}
