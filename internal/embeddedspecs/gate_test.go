package embeddedspecs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// exempt lists the functions that read a directory of JSON specs without going through
// [embeddedspecs.Parse], keyed by "<path>:<func>" with the reason.
var exempt = map[string]string{}

// TestSpecDirectoriesAreReadThroughParse is the gate for this package's reason to exist.
// Dependencies and playbooks carried byte-for-byte copies of the read-decode-reserve-sort
// loop, and extensions a third, so the loop is owned here and forbidden elsewhere.
//
// The check is syntactic and function-scoped: it fails a production function that both
// lists a directory with fs.ReadDir and decodes with json.Unmarshal. That is how the loop
// is written in one sitting; it does not see a loop split across functions, and it is not
// meant to. It stops the sequence being re-derived by someone who does not know this
// package is here.
func TestSpecDirectoriesAreReadThroughParse(t *testing.T) {
	seen := map[string]bool{}
	eachProductionFunc(t, func(rel string, fn *ast.FuncDecl) {
		calls := selectorCalls(fn)
		if !calls["fs.ReadDir"] || !calls["json.Unmarshal"] {
			return
		}
		key := rel + ":" + fn.Name.Name
		seen[key] = true
		if _, ok := exempt[key]; ok {
			return
		}
		t.Errorf("%s lists a directory with fs.ReadDir and decodes it with json.Unmarshal. "+
			"Read bundled specs with embeddedspecs.Parse, which also owns the reserved-name "+
			"and ordering rules. If this is a different job, add it to exempt with the reason.", key)
	})
	for key, reason := range exempt {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exempt: %q carries no reason", key)
		}
		if !seen[key] {
			t.Errorf("exempt: %q no longer reads a directory and decodes it; delete the entry", key)
		}
	}
}

// eachProductionFunc calls visit for every function declaration in the shipped tree, with
// its slash-separated path relative to the repository root. Test files, dot directories,
// vendored code and testdata are not the shipped tree, and neither is this package.
func eachProductionFunc(t *testing.T, visit func(rel string, fn *ast.FuncDecl)) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relPath)
		if strings.HasPrefix(rel, "internal/embeddedspecs/") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				visit(rel, fn)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
}

// selectorCalls reports every package-qualified call under n, as "pkg.Func".
func selectorCalls(n ast.Node) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok {
				found[pkg.Name+"."+sel.Sel.Name] = true
			}
		}
		return true
	})
	return found
}
