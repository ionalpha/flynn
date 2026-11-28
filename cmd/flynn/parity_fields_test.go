package main

import (
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// unreadFields are the fields no code selects, each with why that is correct.
var unreadFields = map[string]string{}

// TestEveryStructFieldIsRead type-checks the package and fails on a field that is set or
// declared and never read, which is how an option gets accepted and ignored. Structs with
// field tags are skipped, because an encoder reads those by reflection.
func TestEveryStructFieldIsRead(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if path, file, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			exports[path] = file
		}
	}

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		// Only the files this platform builds, so the check matches what was listed.
		if strings.HasSuffix(name, "_test.go") || !buildsHere(t, name) {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(exports[path])
	})}
	pkg, err := conf.Check("main", fset, files, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}

	written := map[*ast.SelectorExpr]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && as.Tok == token.ASSIGN {
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						written[sel] = true
					}
				}
			}
			return true
		})
	}
	read := map[types.Object]bool{}
	for sel, s := range info.Selections {
		// Walk the whole path, so an embedded struct reached through counts as read.
		typ := s.Recv()
		path := s.Index()
		for i, idx := range path {
			if p, ok := typ.Underlying().(*types.Pointer); ok {
				typ = p.Elem()
			}
			st, ok := typ.Underlying().(*types.Struct)
			if !ok || (i == len(path)-1 && (s.Kind() != types.FieldVal || written[sel])) {
				break
			}
			read[st.Field(idx).Origin()] = true
			typ = st.Field(idx).Type()
		}
	}

	checked := 0
	for ident, obj := range info.Defs {
		tn, ok := obj.(*types.TypeName)
		if !ok || obj.Pkg() != pkg {
			continue
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok || tagged(st) {
			continue
		}
		for i := range st.NumFields() {
			f := st.Field(i)
			name := ident.Name + "." + f.Name()
			checked++
			switch {
			// An embedded field is used through the methods it promotes.
			case f.Name() == "_" || f.Embedded() || read[f] || unreadFields[name] != "":
			default:
				t.Errorf("%s: %s is never read; wire it up or delete it", fset.Position(f.Pos()), name)
			}
		}
	}
	if checked < 100 {
		t.Fatalf("checked only %d fields: the walk is broken", checked)
	}
}

func buildsHere(t *testing.T, name string) bool {
	t.Helper()
	ok, err := build.Default.MatchFile(".", name)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func tagged(st *types.Struct) bool {
	for i := range st.NumFields() {
		if reflect.StructTag(st.Tag(i)) != "" {
			return true
		}
	}
	return false
}
