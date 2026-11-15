// Package portregister keeps docs/HOST_BOUNDARY.md and the tree in agreement. Every
// exported interface in the public band must be accounted for in the register, and the
// register may not name a package that no longer exists.
//
// A register written once decays: ports land, each reasonable, until the document
// describes a tree that is gone and its record of what Flynn can do without a host is
// evidence of nothing. Nobody finds out until someone runs the binary without a host,
// so the mechanical rules are enforced in code.
//
// # What is checked
//
// Membership and code references only. Whether a seam is shipped, justified or staged
// is a decision a person makes and defends in prose, and a checker that guessed would
// be wrong. So this asks whether every public-band interface is written down somewhere,
// and whether every package the register names still exists.
//
// # Where an interface is accounted for
//
// A verdict row: the interface is a seam Flynn defers through, and the row carries its
// shipped producer, wiring site and verdict.
//
// An optional capability: an interface a producer may also implement, found by type
// assertion, where not implementing it has a defined fallback (resource.KeyLister lets
// a backend list keys without copying records). Nothing is deferred, but the register
// lists the fallback, because what happens when it is absent is the question a verdict
// answers.
//
// Not a deferral: a handle a port returns (clock.Timer, observe.Span) or an interface
// Flynn implements for another consumer (reconcile.Reconciler). Each carries a one-line
// reason, because an unexplained exclusion loses coverage with nobody deciding it.
//
// Only the public band is scanned, as in internal/hostneutral: every package outside
// internal/, without _test.go files. That is what a host depends on; an interface under
// internal/ has no outside implementer.
package portregister

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Seam is one exported interface in the public band, named as the register names it:
// package name, dot, type. Path and Line point a failure at the declaration that needs
// a row.
type Seam struct {
	Name string
	Path string
	Line int
}

// Finding is one way the register and the tree disagree.
type Finding struct {
	// Subject is the seam or the register entry the finding is about.
	Subject string
	// Where locates it: a source position for a seam, the register for an entry.
	Where string
	// Detail says what is wrong and what to do, for someone who has just added an
	// interface and never read this package.
	Detail string
}

func (f Finding) String() string { return f.Where + ": " + f.Subject + ": " + f.Detail }

// skipDirs are pruned anywhere in the walk. They match hostneutral's list, because the
// two checks read one boundary from each end.
var skipDirs = map[string]bool{
	".git": true, ".worktrees": true, "testdata": true,
	"vendor": true, "node_modules": true, "internal": true,
}

// Seams returns every exported interface declared in the public band under root, in
// walk order.
func Seams(root string) ([]Seam, error) {
	var out []Seam
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// WalkDir returns only paths under root, so a prefix trim gives the relative
		// form. filepath.Rel would add an error branch no test can reach, and dead code
		// in a gate has never run.
		rel := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(path, root), string(filepath.Separator)))
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			// A file that does not parse fails the build, which reports it.
			return nil //nolint:nilerr // parse errors are reported by the build
		}
		out = append(out, interfacesIn(fset, file, rel)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// interfacesIn collects the exported interface declarations in one parsed file.
func interfacesIn(fset *token.FileSet, file *ast.File, rel string) []Seam {
	var out []Seam
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || !ts.Name.IsExported() {
			return true
		}
		if _, ok := ts.Type.(*ast.InterfaceType); !ok {
			return true
		}
		out = append(out, Seam{
			Name: file.Name.Name + "." + ts.Name.Name,
			Path: rel,
			Line: fset.Position(ts.Name.Pos()).Line,
		})
		return true
	})
	return out
}

// Register is what docs/HOST_BOUNDARY.md accounts for.
type Register struct {
	// Rows are the code spans in the first column of a verdict table, keyed by the
	// span's text, with the row's verdict.
	Rows map[string]string
	// Accounted is every code span in the document. Membership is checked against it,
	// because a seam may be named in the prose under a row (the four halves of the
	// approval stack are one deferral and one row), and a row per interface would hide
	// the deferral.
	Accounted map[string]bool
	// Verdicts counts each verdict word, so a test can assert no undefined word
	// appeared.
	Verdicts map[string]int
	// Gaps are the subjects of rows whose verdict is gap, with the row's own text, so
	// a gap that says nothing about what to build can be refused.
	Gaps map[string]string
}

// accounts reports whether the register mentions seam in any spelling the document
// uses: canonical, by import path where that disambiguates (`memory/digest.Pusher`), or
// bare for siblings grouped on one row (`state.SessionStore` / `SkillStore` /
// `MemoryStore`).
//
// A bare name can match by coincidence: another row's `Validator` covering a
// `Validator` in a package nobody wrote about. The check exists to catch the interface
// nobody thought about, and nobody writes the name of an interface they have not
// thought about, so the trade is accepted.
func (r Register) accounts(seam string) bool {
	if r.Accounted[seam] {
		return true
	}
	_, bare, _ := strings.Cut(seam, ".")
	if r.Accounted[bare] {
		return true
	}
	for span := range r.Accounted {
		if strings.HasSuffix(span, "/"+seam) {
			return true
		}
	}
	return false
}

// codeSpan matches a `backticked` run.
var codeSpan = regexp.MustCompile("`([^`]+)`")

// tableRow matches a markdown table row: leading pipe, cells, trailing pipe.
var tableRow = regexp.MustCompile(`^\|(.+)\|\s*$`)

// knownVerdicts is the closed vocabulary; any other word is a finding. A reader knows
// what each verdict obliges, and a row carrying "partial" obliges nothing.
var knownVerdicts = map[string]bool{"shipped": true, "justified": true, "staged": true, "gap": true}

// ParseRegister reads the register at path.
func ParseRegister(path string) (Register, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the caller names the register; there is no untrusted input here
	if err != nil {
		return Register{}, err
	}
	reg := Register{
		Rows:      map[string]string{},
		Accounted: map[string]bool{},
		Verdicts:  map[string]int{},
		Gaps:      map[string]string{},
	}
	for _, line := range strings.Split(string(b), "\n") {
		for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
			reg.Accounted[m[1]] = true
		}
		m := tableRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		// Split never returns an empty slice. A one-celled line reads its only cell as
		// the verdict, fails the check, and falls through as prose.
		cells := strings.Split(m[1], "|")
		verdict := strings.TrimSpace(cells[len(cells)-1])
		if !knownVerdicts[verdict] {
			continue
		}
		reg.Verdicts[verdict]++
		for _, span := range codeSpan.FindAllStringSubmatch(cells[0], -1) {
			reg.Rows[span[1]] = verdict
			if verdict == "gap" {
				reg.Gaps[span[1]] = strings.TrimSpace(m[1])
			}
		}
	}
	return reg, nil
}

// Check reports every way the register and the tree under root disagree. The two
// directions are separate findings: a seam with nothing written about it needs a
// message saying what to write, and an entry naming a gone package is a rename that
// took the row's meaning, which nobody notices because the prose still reads fine.
func Check(root, registerPath string) ([]Finding, error) {
	seams, err := Seams(root)
	if err != nil {
		return nil, err
	}
	reg, err := ParseRegister(registerPath)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, s := range seams {
		if reg.accounts(s.Name) {
			continue
		}
		out = append(out, Finding{
			Subject: s.Name,
			Where:   fmt.Sprintf("%s:%d", s.Path, s.Line),
			Detail: "an exported interface in the public band with nothing about it in " + filepath.ToSlash(registerPath) + ". " +
				"Give it a verdict row (shipped: Flynn implements it and the binary wires it; " +
				"justified: Flynn ships none on purpose, the reason is in the doc comment, and its absence stalls or refuses by name; " +
				"staged: Flynn ships it and it is off by default, so name the switch and what makes it the default). " +
				"If nothing is deferred through it, say so instead: an optional capability a producer may also implement goes in that table with its fallback, " +
				"and a handle type or an interface Flynn itself implements goes in the not-a-deferral list with a one-line reason",
		})
	}
	out = append(out, missingPackages(reg, root, registerPath)...)
	for subject, row := range reg.Gaps {
		if len(strings.Fields(row)) < 8 {
			out = append(out, Finding{
				Subject: subject,
				Where:   filepath.ToSlash(registerPath),
				Detail:  "a gap row has to say what to build; a gap nobody wrote down is how one becomes permanent without anyone deciding it should",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Where < out[j].Where })
	return out, nil
}

// missingPackages reports register entries whose package is gone. Only the package
// half is resolved, because a row names things that are not declarations (a flag, a
// struct field, a directory). A vanished package directory is unambiguous, and a
// rename is the drift that happens, since it leaves the prose reading fine.
func missingPackages(reg Register, root, registerPath string) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for span := range reg.Rows {
		pkg, _, ok := strings.Cut(span, ".")
		if !ok || seen[pkg] || strings.ContainsAny(pkg, " /") {
			continue
		}
		seen[pkg] = true
		if packageExists(root, pkg) {
			continue
		}
		out = append(out, Finding{
			Subject: span,
			Where:   filepath.ToSlash(registerPath),
			Detail:  "the register names package " + pkg + " and the tree has no such package; a rename took the row's meaning with it and the prose still reads fine",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// packageExists reports whether a public-band directory declares package pkg. It reads
// the declaration, because directory and package names diverge (memory/distil,
// skill/skilltool).
func packageExists(root, pkg string) bool {
	found := false
	fset := token.NewFileSet()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		// An unreadable directory cannot show the package is absent; stopping on it
		// would report a live package as gone.
		if err != nil || found {
			return nil //nolint:nilerr // an unreadable directory cannot say the package is absent
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && d.Name() != "internal" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if f, perr := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly); perr == nil && f.Name.Name == pkg {
			found = true
		}
		// A file that does not parse cannot name its package, and the build reports it.
		return nil
	})
	return found
}
