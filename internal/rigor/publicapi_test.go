package rigor_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// leakAllowlist is the set of internal types that public API is known to name. A
// key is "<declaration> -> <internal type>", exactly as the audit prints it. An
// entry is a debt with an owner: its value says which task retires it, and the
// test fails on an entry that no longer leaks, so the list only shrinks.
var leakAllowlist = map[string]string{
	"agent.Config.Agent -> internal/archetype.Spec":                     "task 446269ed: a host can pass review.Archetype() but cannot construct its own Spec",
	"review.Archetype -> internal/archetype.Spec":                       "task 446269ed: the same type as agent.Config.Agent",
	"extension.Origin.Identity -> internal/sigstore.Identity":           "task 446269ed: a host cannot pin its own release origin",
	"extension.ReleaseResolver.Downloader -> internal/fetch.Downloader": "task 446269ed: only fetch.New, which a host cannot call, builds one",
}

// TestPublicAPINamesNoInternalType fails when an exported signature, field or
// method of a public package names a type declared under internal/. Such an API
// compiles, but a host outside the module cannot spell the type, construct a
// value of it, or implement it, so the declaration is unusable and the type is in
// the compatibility promise whether or not anyone meant it to be.
//
// The public set is every package of the module that is not under internal/, not a
// command, and has shipped Go files: the stable and domain tiers of
// ARCHITECTURE.md, since both are importable. Types are read from compiler export
// data, so the audit sees what an importer sees, including aliases.
//
// A type reached through an exported alias of a public package (type Spec =
// internal.Spec) is the sanctioned way to re-export; the alias is not a leak, but
// the members of the aliased type are audited as the alias's own API.
func TestPublicAPINamesNoInternalType(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	module := modulePath(t, root)

	pkgs := listPackages(t, root)
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		p, ok := pkgs[path]
		if !ok || p.Export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(p.Export)
	})

	a := &leakAudit{module: module, found: map[string]bool{}}
	var public []string
	for path, p := range pkgs {
		if !p.DepOnly && a.isPublic(path) && p.Name != "main" && len(p.GoFiles) > 0 {
			public = append(public, path)
		}
	}
	slices.Sort(public)
	if len(public) == 0 {
		t.Fatal("found no public packages; the audit would pass vacuously")
	}
	var audited []*types.Package
	for _, path := range public {
		pkg, err := imp.Import(path)
		if err != nil {
			t.Fatalf("import %s: %v", path, err)
		}
		audited = append(audited, pkg)
	}
	for _, pkg := range audited {
		a.noteAliases(pkg)
	}
	for _, pkg := range audited {
		a.auditPackage(pkg)
	}

	for _, key := range slices.Sorted(maps.Keys(a.found)) {
		if _, ok := leakAllowlist[key]; !ok {
			t.Errorf("public API names an internal type: %s\n\tmove the type to a public package, re-export it as an alias, or allowlist it with the task that will", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(leakAllowlist)) {
		if !a.found[key] {
			t.Errorf("leakAllowlist entry %q no longer leaks; delete it", key)
		}
	}
}

// listedPackage is the part of `go list -json` the audit reads.
type listedPackage struct {
	ImportPath string
	Name       string
	Export     string
	GoFiles    []string
	DepOnly    bool
}

// listPackages asks the go command for every package of the module and the export
// data of everything they import, keyed by import path. Export data is compiled
// output, so a package that does not build fails the audit instead of passing it.
func listPackages(t *testing.T, root string) map[string]listedPackage {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps",
		"-json=ImportPath,Name,Export,GoFiles,DepOnly", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	pkgs := map[string]listedPackage{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs[p.ImportPath] = p
	}
	return pkgs
}

// leakAudit walks the exported API of packages and records every internal type it
// names, as "<declaration> -> <internal type>".
type leakAudit struct {
	module string
	found  map[string]bool
	// reexported holds the internal types some public package names through an
	// exported alias, as "<import path>.<name>". An importer can write those, so
	// they are not leaks wherever they appear.
	reexported map[string]bool
	// seen guards the walk against recursive types and repeated visits of the same
	// type under the same declaration.
	seen map[seenKey]bool
}

type seenKey struct {
	decl string
	typ  types.Type
}

// isPublic reports whether an import path of this module is part of the public
// set. Commands are excluded by the caller through the package name.
func (a *leakAudit) isPublic(path string) bool {
	rel, ok := strings.CutPrefix(path, a.module)
	if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
		return false
	}
	rel = strings.TrimPrefix(rel, "/")
	if rel == "e2e" || strings.HasPrefix(rel, "cmd/") {
		return false
	}
	return !isInternalPath(path)
}

// isInternalPath reports whether an import path has an internal element.
func isInternalPath(path string) bool {
	return slices.Contains(strings.Split(path, "/"), "internal")
}

// noteAliases records the internal types pkg re-exports through an exported alias.
// It runs over every public package before any is audited, because an alias in one
// package sanctions the type in all of them.
func (a *leakAudit) noteAliases(pkg *types.Package) {
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || !obj.Exported() || !obj.IsAlias() {
			continue
		}
		if target, ok := types.Unalias(obj.Type()).(*types.Named); ok && isInternalPath(pkgPath(target.Obj())) {
			if a.reexported == nil {
				a.reexported = map[string]bool{}
			}
			a.reexported[pkgPath(target.Obj())+"."+target.Obj().Name()] = true
		}
	}
}

func (a *leakAudit) auditPackage(pkg *types.Package) {
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		decl := pkg.Name() + "." + name
		switch o := obj.(type) {
		case *types.TypeName:
			a.auditTypeName(decl, o)
		default:
			a.visit(decl, obj.Type())
		}
	}
}

// auditTypeName audits a declared type: its underlying shape, any type parameter
// constraints, and its declared methods.
func (a *leakAudit) auditTypeName(decl string, obj *types.TypeName) {
	t := obj.Type()
	if alias, ok := t.(*types.Alias); ok {
		// The alias is the public name (see noteAliases). Audit what it points at,
		// so an aliased internal type cannot carry further internal types out with
		// it.
		a.members(decl, types.Unalias(alias))
		return
	}
	a.members(decl, t)
}

// members audits the API that hangs off a named or composite type: struct
// fields, interface methods, type parameters and declared methods.
func (a *leakAudit) members(decl string, t types.Type) {
	key := seenKey{decl, t}
	if a.seen == nil {
		a.seen = map[seenKey]bool{}
	}
	if a.seen[key] {
		return
	}
	a.seen[key] = true

	switch t := t.(type) {
	case *types.Named:
		if tp := t.TypeParams(); tp != nil {
			for i := range tp.Len() {
				a.visit(decl, tp.At(i).Constraint())
			}
		}
		a.visit(decl, t.Underlying())
		for m := range t.Methods() {
			if m.Exported() {
				a.visit(decl+"."+m.Name(), m.Type())
			}
		}
	default:
		a.visit(decl, t)
	}
}

// visit records every internal type reachable from t through the shapes an
// importer can use: it does not enter the members of another package's named
// type, because that package is audited under its own declarations.
func (a *leakAudit) visit(decl string, t types.Type) {
	if t == nil {
		return
	}
	switch t := t.(type) {
	case *types.Alias:
		if a.isPublic(pkgPath(t.Obj())) {
			// An importer can write this name; the aliased type's own members
			// are audited where the alias is declared.
			for i := range t.TypeArgs().Len() {
				a.visit(decl, t.TypeArgs().At(i))
			}
			return
		}
		a.visit(decl, types.Unalias(t))
	case *types.Named:
		path := pkgPath(t.Obj())
		if isInternalPath(path) && !a.reexported[path+"."+t.Obj().Name()] {
			a.found[decl+" -> "+shortPath(a.module, path)+"."+t.Obj().Name()] = true
		}
		for i := range t.TypeArgs().Len() {
			a.visit(decl, t.TypeArgs().At(i))
		}
		// An unexported type of a public package still hands its exported members
		// to whoever receives a value of it.
		if !t.Obj().Exported() && a.isPublic(path) {
			a.members(decl+" ("+t.Obj().Name()+")", t)
		}
	case *types.Pointer:
		a.visit(decl, t.Elem())
	case *types.Slice:
		a.visit(decl, t.Elem())
	case *types.Array:
		a.visit(decl, t.Elem())
	case *types.Chan:
		a.visit(decl, t.Elem())
	case *types.Map:
		a.visit(decl, t.Key())
		a.visit(decl, t.Elem())
	case *types.Tuple:
		for v := range t.Variables() {
			a.visit(decl, v.Type())
		}
	case *types.Signature:
		if tp := t.TypeParams(); tp != nil {
			for i := range tp.Len() {
				a.visit(decl, tp.At(i).Constraint())
			}
		}
		a.visit(decl, t.Params())
		a.visit(decl, t.Results())
	case *types.Struct:
		for f := range t.Fields() {
			// An embedded field promotes its members whatever its own name's case.
			if f.Exported() || f.Embedded() {
				a.visit(decl+"."+f.Name(), f.Type())
			}
		}
	case *types.Interface:
		for m := range t.ExplicitMethods() {
			if m.Exported() {
				a.visit(decl+"."+m.Name(), m.Type())
			}
		}
		for i := range t.NumEmbeddeds() {
			a.visit(decl, t.EmbeddedType(i))
		}
	case *types.Union:
		for i := range t.Len() {
			a.visit(decl, t.Term(i).Type())
		}
	case *types.TypeParam:
		// The constraint is visited where the parameter is declared.
	}
}

func pkgPath(obj types.Object) string {
	if obj.Pkg() == nil {
		return ""
	}
	return obj.Pkg().Path()
}

// shortPath trims the module prefix so keys read as the repository lays out.
func shortPath(module, path string) string {
	return strings.TrimPrefix(path, module+"/")
}

// fakeImporter resolves import paths to packages type-checked from source in the
// same test.
type fakeImporter map[string]*types.Package

func (f fakeImporter) Import(path string) (*types.Package, error) {
	if p, ok := f[path]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown import %s", path)
}

func checkSource(t *testing.T, imp fakeImporter, path, src string) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path+".go", src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	pkg, err := (&types.Config{Importer: imp}).Check(path, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("check %s: %v", path, err)
	}
	imp[path] = pkg
	return pkg
}

// TestLeakAuditFindsEveryShapeOfLeak pins what the audit counts as a leak, on
// source small enough to read, so a shape it stops seeing fails here rather than
// passing silently over the real module.
func TestLeakAuditFindsEveryShapeOfLeak(t *testing.T) {
	const internalSrc = `package in
type T struct{ N int }
func (T) Self() T { return T{} }
type Box[V any] struct{ V V }
type Other struct{}
type Spec struct{ O Other }
`
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"clean", `package pub
type C struct{ W error; N int }
func F(c C) error { return nil }`, nil},
		{"field", `package pub
import "m/internal/in"
type C struct{ T *in.T }`, []string{"pub.C.T -> internal/in.T"}},
		{"unexported field is not API", `package pub
import "m/internal/in"
type C struct{ t in.T }`, nil},
		{"embedded field promotes its members", `package pub
import "m/internal/in"
type C struct{ in.T }`, []string{"pub.C.T -> internal/in.T"}},
		{"function result", `package pub
import "m/internal/in"
func F() (int, []in.T) { return 0, nil }`, []string{"pub.F -> internal/in.T"}},
		{"method parameter", `package pub
import "m/internal/in"
type C struct{}
func (C) M(m map[string]in.T) {}`, []string{"pub.C.M -> internal/in.T"}},
		{"unexported method is not API", `package pub
import "m/internal/in"
type C struct{}
func (C) m(in.T) {}`, nil},
		{"interface method", `package pub
import "m/internal/in"
type I interface{ M() in.T }`, []string{"pub.I.M -> internal/in.T"}},
		{"type argument", `package pub
import "m/internal/in"
type C struct{ B in.Box[string] }`, []string{"pub.C.B -> internal/in.Box"}},
		{"internal type argument", `package pub
import "m/internal/in"
type Box[V any] struct{ V V }
type C struct{ B Box[in.T] }`, []string{"pub.C.B -> internal/in.T"}},
		{"var and const", `package pub
import "m/internal/in"
var V in.T`, []string{"pub.V -> internal/in.T"}},
		{"exported alias is a sanctioned name", `package pub
import "m/internal/in"
type T = in.T
type C struct{ T *T }
func F(T) {}`, nil},
		{"alias exposes the aliased type's own members", `package pub
import "m/internal/in"
type Spec = in.Spec
func F(Spec) {}`, []string{"pub.Spec.O -> internal/in.Other"}},
		{"unexported type hands out its members", `package pub
import "m/internal/in"
type hidden struct{}
func (hidden) M() in.T { return in.T{} }
func New() *hidden { return nil }`, []string{"pub.New (hidden).M -> internal/in.T"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imp := fakeImporter{}
			checkSource(t, imp, "m/internal/in", internalSrc)
			pkg := checkSource(t, imp, "m/pub", tc.src)

			a := &leakAudit{module: "m", found: map[string]bool{}}
			a.noteAliases(pkg)
			a.auditPackage(pkg)
			got := slices.Sorted(maps.Keys(a.found))
			if !slices.Equal(got, tc.want) {
				t.Errorf("leaks = %q, want %q", got, tc.want)
			}
		})
	}
}
