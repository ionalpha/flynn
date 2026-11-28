package rigor_test

import (
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// closeErrExemptions names the non-test files that discard the Close error of a file they
// wrote, each with the reason. The test fails on an entry whose file no longer does, so
// the list only shrinks.
var closeErrExemptions = map[string]string{}

// writeFlagMask is the set of os.OpenFile flags that make a handle writable or writing.
const writeFlagMask = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC

// writeMethods are the *os.File methods that put bytes into the file.
var writeMethods = map[string]bool{"Write": true, "WriteString": true, "WriteAt": true, "ReadFrom": true, "Truncate": true}

// closeFinding is one written file whose Close error is thrown away.
type closeFinding struct {
	line int
	pos  string
	name string
}

// stdOnlyImporter resolves the standard library from source and gives every other import
// an empty, complete package. The check needs the real *os.File and nothing else, so a
// package of this module is never loaded; the type errors that follow from the empty
// packages are ignored, and an expression whose type is lost simply is not matched.
type stdOnlyImporter struct {
	std  types.Importer
	fake map[string]*types.Package
}

func (i *stdOnlyImporter) Import(path string) (*types.Package, error) {
	if !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
		if p, err := i.std.Import(path); err == nil {
			return p, nil
		}
	}
	if p, ok := i.fake[path]; ok {
		return p, nil
	}
	p := types.NewPackage(path, filepath.Base(path))
	p.MarkComplete()
	i.fake[path] = p
	return p, nil
}

func newCloseImporter() types.Importer {
	return &stdOnlyImporter{std: importer.ForCompiler(token.NewFileSet(), "source", nil), fake: map[string]*types.Package{}}
}

func allBlank(lhs []ast.Expr) bool {
	for _, e := range lhs {
		if id, ok := e.(*ast.Ident); !ok || id.Name != "_" {
			return false
		}
	}
	return true
}

type discardAt struct {
	obj types.Object
	pos token.Pos
}

// discardedCloses returns the written files in src whose Close error is discarded. A file
// counts as written when it comes from os.Create, os.CreateTemp or an os.OpenFile with a
// write flag, and the same declaration calls a write method on it or hands it to another
// call (io.Copy, fmt.Fprintf, a buffered writer). A file opened only to reserve a name and
// closed without being written is not reported. A discard is f.Close() as a statement,
// _ = f.Close(), defer f.Close(), or the same inside a deferred func literal.
func discardedCloses(t *testing.T, imp types.Importer, filename string, src any) []closeFinding {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: imp, Error: func(error) {}, FakeImportC: true}
	_, _ = conf.Check(f.Name.Name, fset, []*ast.File{f}, info)

	isOSFile := func(e ast.Expr) bool {
		tv, ok := info.Types[e]
		if !ok {
			return false
		}
		ptr, ok := tv.Type.(*types.Pointer)
		if !ok {
			return false
		}
		named, ok := ptr.Elem().(*types.Named)
		return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "os" && named.Obj().Name() == "File"
	}
	// writesFile reports whether call is os.Create, os.CreateTemp or a writing os.OpenFile.
	writesFile := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		fn, ok := info.Uses[sel.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "os" {
			return false
		}
		switch fn.Name() {
		case "Create", "CreateTemp":
			return true
		case "OpenFile":
			if len(call.Args) < 2 {
				return false
			}
			if v := info.Types[call.Args[1]].Value; v != nil && v.Kind() == constant.Int {
				n, _ := constant.Int64Val(v)
				return n&int64(writeFlagMask) != 0
			}
			return true // a flag that is not constant may be writing
		}
		return false
	}
	// closeTarget returns the object whose Close the call makes, if it is an os.File.
	closeTarget := func(call *ast.CallExpr) types.Object {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Close" || len(call.Args) != 0 || !isOSFile(sel.X) {
			return nil
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return nil
		}
		return info.Uses[id]
	}

	var out []closeFinding
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		opened := map[types.Object]bool{}
		written := map[types.Object]bool{}
		var discards []discardAt
		discard := func(e ast.Expr) {
			if call, ok := e.(*ast.CallExpr); ok {
				if obj := closeTarget(call); obj != nil {
					discards = append(discards, discardAt{obj, call.Pos()})
				}
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Rhs) == 1 {
					if call, ok := n.Rhs[0].(*ast.CallExpr); ok && writesFile(call) {
						if id, ok := n.Lhs[0].(*ast.Ident); ok {
							if obj := info.ObjectOf(id); obj != nil {
								opened[obj] = true
							}
						}
					}
					if allBlank(n.Lhs) {
						discard(n.Rhs[0])
					}
				}
			case *ast.ExprStmt:
				discard(n.X)
			case *ast.DeferStmt:
				discard(n.Call)
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && writeMethods[sel.Sel.Name] && isOSFile(sel.X) {
					if id, ok := sel.X.(*ast.Ident); ok {
						written[info.Uses[id]] = true
					}
				}
				for _, a := range n.Args {
					if id, ok := a.(*ast.Ident); ok && isOSFile(id) {
						written[info.Uses[id]] = true
					}
				}
			}
			return true
		})
		for _, d := range discards {
			if opened[d.obj] && written[d.obj] {
				p := fset.Position(d.pos)
				out = append(out, closeFinding{line: p.Line, pos: p.String(), name: d.obj.Name()})
			}
		}
	}
	return out
}

// TestWrittenFilesKeepTheirCloseError fails when production code writes through a file it
// created and throws away the error from closing it. A write can succeed into the page
// cache and fail only at close, so a discarded close installs a truncated file without a
// sign. Use [closeerr.Keep] deferred on a named error result, or return the Close error.
func TestWrittenFilesKeepTheirCloseError(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	imp := newCloseImporter()
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
		data, err := os.ReadFile(path) //nolint:gosec // path comes from walking the repository
		if err != nil {
			return err
		}
		// Type-checking loads the standard library from source, which is slow, so only a
		// file that names one of the constructors is checked.
		if !strings.Contains(string(data), "os.Create") && !strings.Contains(string(data), "os.OpenFile") {
			return nil
		}
		hits := discardedCloses(t, imp, path, data)
		if len(hits) == 0 {
			return nil
		}
		if _, ok := closeErrExemptions[rel]; ok {
			found[rel] = true
			return nil
		}
		for _, h := range hits {
			bad = append(bad, h.name+" closed at "+h.pos)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s: the file was written and its Close error is discarded; use defer closeerr.Keep(&err, f) on a named error result, or return the Close error", b)
	}
	for file, reason := range closeErrExemptions {
		if reason == "" {
			t.Errorf("exemption %q has no reason", file)
		}
		if !found[file] {
			t.Errorf("exemption %q no longer discards a written file's Close error: remove it", file)
		}
	}
}

// TestCloseErrorDetectorFlagsTheBadShapes keeps the gate above honest: every way of
// discarding the Close of a written file is found, and a reserved-name open, a read-only
// open and a kept close are not.
func TestCloseErrorDetectorFlagsTheBadShapes(t *testing.T) {
	src := `package p
import ("io"; "os")
func blank(p string, r io.Reader) { f, _ := os.Create(p); _, _ = io.Copy(f, r); _ = f.Close() }
func deferred(p string) { f, _ := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600); defer f.Close(); f.WriteString("x") }
func deferredLit(p string) { f, _ := os.CreateTemp("", "x"); defer func() { _ = f.Close() }(); f.Write(nil) }
func bare(p string) { f, _ := os.OpenFile(p, os.O_RDWR, 0); f.Write(nil); f.Close() }
func reserve(p string) { f, _ := os.CreateTemp("", "x"); name := f.Name(); _ = f.Close(); _ = os.Remove(name) }
func readOnly(p string) { f, _ := os.OpenFile(p, os.O_RDONLY, 0); _, _ = io.ReadAll(f); _ = f.Close() }
func kept(p string) (err error) { f, _ := os.Create(p); f.Write(nil); defer func() { if cerr := f.Close(); err == nil { err = cerr } }(); return nil }
func checked(p string) error { f, _ := os.Create(p); f.Write(nil); return f.Close() }
`
	got := map[int]bool{}
	for _, h := range discardedCloses(t, newCloseImporter(), "x.go", src) {
		got[h.line] = true
	}
	for i, l := range strings.Split(src, "\n") {
		want := strings.HasPrefix(l, "func blank") || strings.HasPrefix(l, "func deferred") || strings.HasPrefix(l, "func bare")
		if got[i+1] != want {
			t.Errorf("line %s %.30q: flagged=%v, want %v", strconv.Itoa(i+1), l, got[i+1], want)
		}
	}
}
