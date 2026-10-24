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

// bareWriteExempt lists the functions allowed to call os.WriteFile directly, keyed by
// "<path>:<func>". os.WriteFile truncates the destination and then writes, so a crash
// between the two leaves an empty or partial file where the previous contents were.
// Each entry is a write where in-place truncation is the behaviour wanted, or where
// the file is not state this program has to get back.
var bareWriteExempt = map[string]string{
	"watch/watch.go:clearMarker":        "edits the user's own source file in place; an atomic replace swaps the inode, which severs hard links, turns a symlink into a regular file, drops ownership, ACLs and extended attributes, and detaches editors and watchers holding the old file",
	"internal/testkit/golden.go:Golden": "test support: rewrites a golden file under testdata/ on -update, where version control is the recovery",
}

// packageLevel names var and const declarations, so a call in a package-level
// initializer is attributed to something in a failure message.
const packageLevel = "<package-level>"

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
	eachProductionDecl(t, func(rel, name string, decl ast.Node) {
		calls := selectorCalls(decl)
		if !calls["os.Rename"] {
			return
		}
		staged := ""
		for creator := range tempCreators {
			if calls[creator] {
				staged = creator
				break
			}
		}
		if staged == "" {
			return
		}
		key := rel + ":" + name
		if _, ok := exempt[key]; ok {
			return
		}
		t.Errorf("%s stages content with %s and commits it with os.Rename. "+
			"Write it through fsatomic.WriteFile (bytes) or fsatomic.WriteStream (a producer), "+
			"which fsyncs the contents and the rename. If the rename is not committing content "+
			"this process wrote, add it to exempt with the reason.", key, staged)
	})
}

// TestDurableWritesAreNotBareWriteFile fails a production function that calls
// os.WriteFile. It truncates the destination before writing, so a crash or a full disk
// partway through destroys the previous contents, and no fsync follows to make what
// was written durable. WriteFile in this package is the same call with both fixed.
//
// Like the staging gate it is syntactic: it matches the selector os.WriteFile, so it
// does not see an alias for the os package or a function value that wraps the call. It
// stops the mistake being made by someone who does not know this package is here; it
// does not defeat someone who does.
func TestDurableWritesAreNotBareWriteFile(t *testing.T) {
	eachProductionDecl(t, func(rel, name string, decl ast.Node) {
		if !selectorCalls(decl)["os.WriteFile"] {
			return
		}
		key := rel + ":" + name
		if _, ok := bareWriteExempt[key]; ok {
			return
		}
		t.Errorf("%s calls os.WriteFile, which truncates the destination and never syncs it. "+
			"Use fsatomic.WriteFile (bytes) or fsatomic.WriteStream (a producer). If a "+
			"truncating in-place write is what this site needs, add it to bareWriteExempt "+
			"with the reason.", key)
	})
}

// TestExemptionsAreLive keeps the allowlists honest: an entry that no longer names a
// function that does the thing it is excused for is stale, and a stale exemption is a
// hole nobody can see. It is the same argument the gates make, applied to the gates.
func TestExemptionsAreLive(t *testing.T) {
	checkLive(t, "exempt", exempt, func(calls map[string]bool) bool {
		staged := false
		for creator := range tempCreators {
			staged = staged || calls[creator]
		}
		return calls["os.Rename"] && staged
	})
	checkLive(t, "bareWriteExempt", bareWriteExempt, func(calls map[string]bool) bool {
		return calls["os.WriteFile"]
	})
}

// checkLive fails every entry in list that is malformed, carries no reason, or names a
// function for which still reports false.
func checkLive(t *testing.T, listName string, list map[string]string, still func(calls map[string]bool) bool) {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()

	for key, reason := range list {
		relPath, funcName, ok := strings.Cut(key, ":")
		if !ok {
			t.Errorf("%s: %q is not in <path>:<func> form", listName, key)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s: %q carries no reason", listName, key)
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(relPath)), nil, 0)
		if err != nil {
			t.Errorf("%s: %q: %v", listName, key, err)
			continue
		}
		found := false
		for _, decl := range file.Decls {
			if declName(decl) == funcName && still(selectorCalls(decl)) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %q no longer does what it is excused for; delete it", listName, key)
		}
	}
}

// eachProductionDecl calls visit for every top-level declaration in the shipped tree,
// with its slash-separated path relative to the repository root and the name the
// allowlists key it by. Test files are skipped, and so is this package, which is the
// implementation the gates protect.
func eachProductionDecl(t *testing.T, visit func(rel, name string, decl ast.Node)) {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, decl := range file.Decls {
			if n := declName(decl); n != "" {
				visit(rel, n, decl)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
}

// declName is the allowlist name of a declaration: a function's own name (methods are
// keyed without their receiver), packageLevel for var and const blocks, and empty for
// declarations that cannot contain a call, such as imports and types.
func declName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Body == nil {
			return ""
		}
		return d.Name.Name
	case *ast.GenDecl:
		if d.Tok == token.VAR || d.Tok == token.CONST {
			return packageLevel
		}
	}
	return ""
}

// selectorCalls reports every package-qualified call under n, as "pkg.Func".
func selectorCalls(n ast.Node) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
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

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	return root
}

func mustRel(t *testing.T, base, path string) string {
	t.Helper()
	rel, err := filepath.Rel(base, path)
	if err != nil {
		t.Fatalf("relativize %s: %v", path, err)
	}
	return rel
}
