package rigor_test

import (
	"bufio"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// qualified matches pkg.Name in comment text, bare or inside a doc link. Name must be
// exported: a lowercase tail is as often a file name ("repl.go") as an identifier.
var qualified = regexp.MustCompile(`\b([a-z][a-z0-9]*)\.([A-Z][A-Za-z0-9_]*)\b`)

// localLink matches a doc link to a name in the comment's own package, [Name] or
// [Name.Method]. Only the leading Name is checked.
var localLink = regexp.MustCompile(`\[([A-Z][A-Za-z0-9_]*)(?:\.[A-Za-z0-9_]+)?\]`)

// pkgInfo is one package of the module: its name and the top-level names its
// shipped files declare.
type pkgInfo struct {
	name  string
	decls map[string]bool
}

// TestCommentsNameDeclaredIdentifiers fails on a comment in shipped code that refers
// to pkg.Name, or links [pkg.Name] or [Name], where pkg is a package of this module
// that declares no Name. A rename the comment did not follow is the commonest way a
// comment goes stale, and the only one a parser can see.
//
// The check is deliberately narrow. A reference is checked only when its package
// resolves unambiguously: an import of the file, the file's own package, or the one
// package in the module with that name when no standard-library package shares it.
// Anything else is left alone, so the gate stays quiet enough to block on.
func TestCommentsNameDeclaredIdentifiers(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	module := modulePath(t, root)

	fset := token.NewFileSet()
	pkgs := map[string]*pkgInfo{} // import path -> package
	type parsed struct {
		file       *ast.File
		importPath string
	}
	var files []parsed

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Dot directories (worktrees, tooling fixtures), vendored code, testdata and
			// scratch space are not the shipped tree.
			if p != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "tmp") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		importPath := module
		if rel != "." {
			importPath = module + "/" + filepath.ToSlash(rel)
		}
		info := pkgs[importPath]
		if info == nil {
			info = &pkgInfo{name: f.Name.Name, decls: map[string]bool{}}
			pkgs[importPath] = info
		}
		for _, name := range topLevelNames(f) {
			info.decls[name] = true
		}
		files = append(files, parsed{file: f, importPath: importPath})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A package name that more than one module package uses, or that the standard
	// library also uses, cannot be resolved from prose alone.
	byName := map[string][]string{}
	for importPath, info := range pkgs {
		byName[info.name] = append(byName[info.name], importPath)
	}
	stdlib := map[string]bool{}
	for name := range byName {
		if _, err := build.Default.Import(name, "", build.FindOnly); err == nil {
			stdlib[name] = true
		}
	}

	for _, pf := range files {
		scope := fileScope(pf.file, pf.importPath, pkgs)
		for _, group := range pf.file.Comments {
			text := group.Text()
			for _, m := range qualified.FindAllStringSubmatchIndex(text, -1) {
				pkgName, ident := text[m[2]:m[3]], text[m[4]:m[5]]
				target, ok := scope[pkgName]
				if !ok {
					paths := byName[pkgName]
					if len(paths) != 1 || stdlib[pkgName] {
						continue
					}
					target = paths[0]
				}
				info := pkgs[target]
				if info == nil || info.decls[ident] {
					continue // outside the module, or declared
				}
				t.Errorf("%s: comment refers to %s.%s, which %s does not declare",
					fset.Position(group.Pos()), pkgName, ident, target)
			}
			for _, m := range localLink.FindAllStringSubmatch(text, -1) {
				if !pkgs[pf.importPath].decls[m[1]] {
					t.Errorf("%s: doc link %s names nothing %s declares",
						fset.Position(group.Pos()), m[0], pf.importPath)
				}
			}
		}
	}
}

// fileScope maps the package names a file can mean to import paths: its own package,
// and every import, whether it is in the module or not. An import outside the module
// maps to its own path, which has no entry in pkgs and so is never checked.
func fileScope(f *ast.File, self string, pkgs map[string]*pkgInfo) map[string]string {
	scope := map[string]string{f.Name.Name: self}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if info := pkgs[p]; info != nil {
			name = info.name
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		scope[name] = p
	}
	return scope
}

// topLevelNames lists the names a file declares at package level, including methods,
// so that pkg.Type and pkg.Func both resolve.
func topLevelNames(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			names = append(names, d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						names = append(names, n.Name)
					}
				}
			}
		}
	}
	return names
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("open go.mod: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}
