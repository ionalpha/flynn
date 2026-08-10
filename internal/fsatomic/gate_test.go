package fsatomic_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// tempCreators are the calls that open the sibling file a hand-rolled atomic write
// stages its content in. Paired with a rename, they are the sequence this package
// exists to own.
var tempCreators = map[string]bool{
	"os.CreateTemp": true,
	"os.MkdirTemp":  true,
	"os.Create":     true,
	"os.OpenFile":   true,
}

// exempt lists the functions that stage and rename for a reason fsatomic does not
// cover, keyed by "<path>:<func>". Each one is a rename of something that is not
// content this process wrote through a writer, which is the only thing WriteFile and
// WriteStream know how to commit.
var exempt = map[string]string{
	"internal/acquire/acquire.go:InstallTo": "renames an extracted directory tree into place; fsatomic commits a file, not a tree",
}

// TestAtomicWritesGoThroughFsatomic is the gate for this package's reason to exist.
// Three call sites independently grew the same write-temp-then-rename sequence, and
// two of them omitted the fsync that makes it durable, so the sequence is now owned
// here and forbidden elsewhere.
//
// The check is syntactic and function-scoped: it fails a production function that
// both opens a staging file and calls os.Rename. That catches the mistake as it is
// actually made, in one function, in one sitting. It does not catch a sequence split
// deliberately across two functions, and it is not meant to: this stops the pattern
// being re-derived by someone who does not know the package is here, which is how all
// three copies arrived.
func TestAtomicWritesGoThroughFsatomic(t *testing.T) {
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
			// Dot directories (worktrees, tooling fixtures), vendored code and testdata
			// are not the shipped tree.
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(mustRel(t, root, path))
		if strings.HasPrefix(rel, "internal/fsatomic/") {
			return nil // the implementation itself
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			calls := selectorCalls(fn.Body)
			if !calls["os.Rename"] {
				continue
			}
			staged := ""
			for creator := range tempCreators {
				if calls[creator] {
					staged = creator
					break
				}
			}
			if staged == "" {
				continue
			}
			key := rel + ":" + fn.Name.Name
			if _, ok := exempt[key]; ok {
				continue
			}
			t.Errorf("%s stages content with %s and commits it with os.Rename. "+
				"Write it through fsatomic.WriteFile (bytes) or fsatomic.WriteStream (a producer), "+
				"which fsyncs the contents and the rename. If the rename is not committing content "+
				"this process wrote, add it to exempt with the reason.", key, staged)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
}

// TestExemptionsAreLive keeps the allowlist honest: an entry that no longer names a
// function that stages and renames is stale, and a stale exemption is a hole nobody
// can see. It is the same argument the gate itself makes, applied to the gate.
func TestExemptionsAreLive(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	fset := token.NewFileSet()

	for key, reason := range exempt {
		relPath, funcName, ok := strings.Cut(key, ":")
		if !ok {
			t.Errorf("exemption %q is not in <path>:<func> form", key)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exemption %q carries no reason", key)
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(relPath)), nil, 0)
		if err != nil {
			t.Errorf("exemption %q: %v", key, err)
			continue
		}
		found := false
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil || fn.Name.Name != funcName {
				continue
			}
			calls := selectorCalls(fn.Body)
			staged := false
			for creator := range tempCreators {
				staged = staged || calls[creator]
			}
			found = calls["os.Rename"] && staged
		}
		if !found {
			t.Errorf("exemption %q no longer stages and renames; delete it", key)
		}
	}
}

// selectorCalls reports every package-qualified call in a body, as "pkg.Func".
func selectorCalls(body *ast.BlockStmt) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		found[pkg.Name+"."+sel.Sel.Name] = true
		return true
	})
	return found
}

func mustRel(t *testing.T, base, path string) string {
	t.Helper()
	rel, err := filepath.Rel(base, path)
	if err != nil {
		t.Fatalf("relativize %s: %v", path, err)
	}
	return rel
}
