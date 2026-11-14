package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// transportScopes are the path prefixes of the code that speaks a protocol on standard
// output. Inside them, nothing may reach stdout except through the guard's frame writer. A
// new transport goes under one of these prefixes, which brings it under the rule without
// anyone remembering to add it; a transport placed elsewhere is not covered, and that is a
// reason to move it.
var transportScopes = []string{
	"internal/ndjson/",
	"internal/stdioguard/",
	"internal/headless",
	"cmd/flynn/headless_",
}

// stdoutExemptions names the files inside a transport scope that touch standard output
// directly, each with the reason. The test fails on an entry that no longer does, so the
// list only shrinks.
var stdoutExemptions = map[string]string{
	"internal/stdioguard/guard_windows.go": "repoints the standard output handle and os.Stdout at the stray pipe, which is how the guard takes stdout away from everything else",
}

// stdoutUse is one place a file reaches standard output other than through a frame writer.
type stdoutUse struct {
	where string
	what  string
}

// stdoutUses returns the direct uses of standard output in src: a reference to os.Stdout or
// syscall.Stdout, a Windows standard-output handle, a fmt.Print call, and the print and
// println built-ins, which write to standard error but are banned alongside them because a
// debugging aid that moves to stdout later should not be able to. Selectors are resolved
// through the file's own import names, so a renamed import does not hide a use.
func stdoutUses(t *testing.T, filename string, src any) []stdoutUse {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
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
	var out []stdoutUse
	record := func(n ast.Node, what string) {
		out = append(out, stdoutUse{where: fset.Position(n.Pos()).String(), what: what})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			path, imported := names[id.Name]
			if !imported {
				return true
			}
			name := x.Sel.Name
			switch {
			case path == "os" && name == "Stdout", path == "syscall" && name == "Stdout":
				record(x, filepath.Base(path)+"."+name)
			case (path == "syscall" || path == "golang.org/x/sys/windows") && name == "STD_OUTPUT_HANDLE":
				record(x, "STD_OUTPUT_HANDLE")
			case path == "fmt" && (name == "Print" || name == "Printf" || name == "Println"):
				record(x, "fmt."+name)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") && id.Obj == nil {
				record(x, id.Name+" built-in")
			}
		}
		return true
	})
	return out
}

func inTransportScope(rel string) bool {
	for _, p := range transportScopes {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// TestTransportCodeNeverWritesStdoutDirectly fails when code in a transport scope reaches
// standard output by any route other than the frame writer that stdioguard owns. The guard
// diverts stray writes, but a transport that printed to stdout itself would be printing
// into the diverted stream and losing the output, or, written before the guard is
// installed, corrupting the frames. The rule keeps the question from arising.
func TestTransportCodeNeverWritesStdoutDirectly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	found := map[string]bool{}
	scanned := 0
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
		if !inTransportScope(rel) {
			return nil
		}
		scanned++
		uses := stdoutUses(t, path, nil)
		if len(uses) == 0 {
			return nil
		}
		if _, ok := stdoutExemptions[rel]; ok {
			found[rel] = true
			return nil
		}
		for _, u := range uses {
			bad = append(bad, u.what+" at "+u.where)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no file fell inside a transport scope: the scope list no longer matches the tree, so the rule checks nothing")
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s: transport code must not write stdout directly; frames go through the writer from stdioguard.Guard.Frames, and diagnostics go to stderr or a log", b)
	}
	for file, reason := range stdoutExemptions {
		if reason == "" {
			t.Errorf("exemption %q has no reason", file)
		}
		if !found[file] {
			t.Errorf("exemption %q no longer touches standard output directly: remove it", file)
		}
	}
}

// frameWriterSources returns the calls to NewWriter from the package at importPath whose
// argument is standard output or a file opened over a standard descriptor, which would
// make a second frame writer beside the guard's.
func frameWriterSources(t *testing.T, filename string, src any, importPath string) []stdoutUse {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	names := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		local := filepath.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		names[local] = p
	}
	qualified := func(e ast.Expr) (path, name string, ok bool) {
		sel, isSel := e.(*ast.SelectorExpr)
		if !isSel {
			return "", "", false
		}
		id, isID := sel.X.(*ast.Ident)
		if !isID {
			return "", "", false
		}
		path, ok = names[id.Name]
		return path, sel.Sel.Name, ok
	}
	var out []stdoutUse
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if path, name, ok := qualified(call.Fun); !ok || path != importPath || name != "NewWriter" {
			return true
		}
		switch arg := call.Args[0].(type) {
		case *ast.SelectorExpr:
			if path, name, ok := qualified(arg); ok && path == "os" && (name == "Stdout" || name == "Stderr") {
				out = append(out, stdoutUse{where: fset.Position(call.Pos()).String(), what: "os." + name})
			}
		case *ast.CallExpr:
			if path, name, ok := qualified(arg.Fun); ok && path == "os" && name == "NewFile" {
				out = append(out, stdoutUse{where: fset.Position(call.Pos()).String(), what: "os.NewFile"})
			}
		}
		return true
	})
	return out
}

// TestNoSecondFrameWriter fails when any production file builds an ndjson.Writer over
// standard output or a file made from a descriptor number. The guard's frame writer is the
// only one that reaches the original stdout; a Writer over os.Stdout would write to the
// diverted stream, and one over os.NewFile(1, ...) would write to it before the guard is
// installed.
func TestNoSecondFrameWriter(t *testing.T) {
	const ndjsonPath = "github.com/ionalpha/flynn/internal/ndjson"
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
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
		for _, u := range frameWriterSources(t, path, nil, ndjsonPath) {
			bad = append(bad, u.what+" at "+u.where)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("ndjson.NewWriter over %s: build the frame writer from stdioguard.Guard.Frames, which is the only writer that reaches the original stdout", b)
	}
}

// TestStdoutDetectorsSeeRenamedAndIndirectUses keeps both rules above honest: an aliased
// import, a handle lookup, a print call and a built-in are all found, an ordinary stderr
// write is not, and a second frame writer is recognised however the file names the import.
func TestStdoutDetectorsSeeRenamedAndIndirectUses(t *testing.T) {
	src := `package p
import (
	"fmt"
	stdos "os"
	"golang.org/x/sys/windows"
)
func a() { fmt.Println("x"); fmt.Fprintln(stdos.Stdout, "x") }
func b() { _, _ = windows.GetStdHandle(windows.STD_OUTPUT_HANDLE) }
func c() { println("x") }
func d() { fmt.Fprintln(stdos.Stderr, "ok"); _ = fmt.Sprint("ok") }
`
	var kinds []string
	for _, u := range stdoutUses(t, "x.go", src) {
		kinds = append(kinds, u.what)
	}
	sort.Strings(kinds)
	want := []string{"STD_OUTPUT_HANDLE", "fmt.Println", "os.Stdout", "println built-in"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("stdout detector found %q, want %q", kinds, want)
	}

	writers := `package p
import (
	nd "github.com/ionalpha/flynn/internal/ndjson"
	"os"
)
func a() { _ = nd.NewWriter(os.Stdout) }
func b() { _ = nd.NewWriter(os.NewFile(1, "stdout")) }
func c(w interface{ Write([]byte) (int, error) }) { _ = nd.NewWriter(w) }
`
	got := frameWriterSources(t, "y.go", writers, "github.com/ionalpha/flynn/internal/ndjson")
	if len(got) != 2 || got[0].what != "os.Stdout" || got[1].what != "os.NewFile" {
		t.Fatalf("frame writer detector found %+v, want os.Stdout and os.NewFile", got)
	}
}
