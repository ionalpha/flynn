package render_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// chromaLexersPkg is the import path this module must never reach from
// non-test code: it parses every one of its lexer definitions at init.
const chromaLexersPkg = "github.com/alecthomas/chroma/v2/lexers"

// TestNoChromaLexersImport is the gate that keeps the cost out. chroma's
// lexers package parses 279 definitions in its package init, and Go runs the
// init of every package linked into a binary, so a single import anywhere in
// non-test code puts that cost back into `flynn --version`. The test walks the
// whole module rather than this package: the cheapest way to reintroduce it is
// from somewhere else.
func TestNoChromaLexersImport(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			if name := d.Name(); name != "." && (strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || p != chromaLexersPkg {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s imports %s: its package init parses every lexer definition in every process that links it. Use the embedded registry in internal/tui/render instead.", filepath.ToSlash(rel), chromaLexersPkg)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// moduleRoot climbs to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
