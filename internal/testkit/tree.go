package testkit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// RepoRoot is the repository root seen from a package two directories below it, which
// is where every architecture gate lives (internal/<pkg>).
func RepoRoot(t TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	return root
}

// EachProductionFunc calls visit for every function with a body in the shipped tree,
// with its slash-separated path relative to root. Test files, dot directories (worktrees,
// tooling fixtures), vendored code and testdata are not the shipped tree. A path under
// one of skip is left out, so a gate can exempt the package that owns the pattern it
// forbids elsewhere.
func EachProductionFunc(t TB, root string, skip []string, visit func(rel string, fn *ast.FuncDecl)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		for _, prefix := range skip {
			if strings.HasPrefix(rel, prefix) {
				return nil
			}
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
