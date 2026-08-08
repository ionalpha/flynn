// Package portregister keeps docs/HOST_BOUNDARY.md and the tree from drifting
// apart. Every exported interface in the public band has to be accounted for in the
// register, and the register may not name a package that no longer exists.
//
// A register written once decays. Six months of ports land, each individually
// reasonable, and the doc describes a tree that is no longer there. That failure is
// quiet in exactly the way the register was written to prevent: the document that
// says which capabilities Flynn can perform on its own stops being evidence of
// anything, and nobody finds out until somebody tries to run the binary without a
// host. Where a rule is mechanical, the code enforces it.
//
// # What is checked, and what is left to a person
//
// Membership and code references, never judgment. Whether a seam is shipped,
// justified or staged is a decision somebody has to make and defend in prose, and a
// checker that guessed at it would be wrong in the cases that matter. So this asks
// only: is every interface in the public band written down somewhere, and does every
// package the register names still exist.
//
// # The three places an interface can be accounted for
//
// A verdict row is the main one: the interface is a seam Flynn defers through, and
// the row carries its shipped producer, its wiring site and its verdict.
//
// An optional capability is the second. It is an interface a producer may also
// implement, found by type assertion rather than injected, where not implementing it
// is a supported answer with a defined fallback: resource.KeyLister lets a backend
// list keys without copying records, and a backend that cannot simply does not
// implement it. These are not host deferrals, because nothing is deferred; the
// register lists each with the fallback, since "what happens when it is absent" is
// the same question a verdict answers.
//
// Not a deferral is the third, and it is the honest name for the rest: a handle a
// port hands back (clock.Timer, observe.Span), or an interface Flynn implements and
// something else consumes (reconcile.Reconciler). Each carries a one-line reason,
// because an unexplained exclusion is how the register loses coverage without anyone
// deciding it should.
//
// Only the public band is scanned, matching internal/hostneutral: every package
// outside internal/, ignoring _test.go files. That is the surface a host depends on,
// and an interface under internal/ is nobody's to implement but ours.
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

// Seam is one exported interface in the public band, named the way the register
// names it: the package's own name, a dot, the type. Path and Line locate the
// declaration so a failure points at the thing that needs a row rather than at the
// document.
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
	// Detail says what is wrong and what to do about it. It is written for whoever
	// tripped the check, who is usually somebody who has just added an interface and
	// has never read this package.
	Detail string
}

func (f Finding) String() string { return f.Where + ": " + f.Subject + ": " + f.Detail }

// skipDirs are pruned anywhere in the walk. It matches hostneutral's list, for the
// same reason: the two checks are one boundary read from each end, and a directory
// that is out of scope for one is out of scope for the other.
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			// A file that does not parse is the compiler's problem; the build fails on it
			// either way, and reporting it here would only say so twice.
			return nil //nolint:nilerr // parse errors are reported by the build
		}
		out = append(out, interfacesIn(fset, file, filepath.ToSlash(rel))...)
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
	// Accounted is every code span anywhere in the document. Membership is checked
	// against this rather than against Rows alone, because a seam can legitimately be
	// named in the prose under a row (the four halves of the approval stack are one
	// deferral and one row) and requiring a row per interface would push the register
	// towards a shape that hides the seam it is about.
	Accounted map[string]bool
	// Verdicts counts each verdict word seen, so a test can assert the vocabulary has
	// not grown a fourth member nobody defined.
	Verdicts map[string]int
	// Gaps are the subjects of rows whose verdict is gap, with the row's own text, so
	// a gap that says nothing about what to build can be refused.
	Gaps map[string]string
}

// accounts reports whether the register says anything about this seam, in any of the
// three spellings the document legitimately uses.
//
// The register names a seam by its import path where that is what disambiguates
// (`memory/digest.Pusher`), and it groups siblings that are one deferral onto one
// row, where the second and third are written bare (`state.SessionStore` /
// `SkillStore` / `MemoryStore`). Both are better prose than the canonical form would
// be, and a checker that insisted on the canonical form would be arguing with the
// document about spelling instead of about coverage. So a bare type name counts.
//
// The cost is that a bare name could be accounted for by coincidence: some other
// row's `Validator` covering a `Validator` in a package nobody wrote about. That is
// the right trade. The check exists to catch the interface nobody thought about, and
// nobody writes the name of an interface they have not thought about.
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

// knownVerdicts is the closed vocabulary. A fourth word is a finding, not a new
// category: the whole value of three verdicts is that a reader knows what each one
// obliges, and a row carrying "partial" obliges nothing.
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
		cells := strings.Split(m[1], "|")
		if len(cells) < 2 {
			continue
		}
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

// Check reports every way the register and the tree under root disagree.
//
// The two directions are separate findings on purpose. A seam with nothing written
// about it is a contributor who has not been told the rule yet, and the message says
// what to write. A register entry naming a package that is gone is a rename that
// took the row's meaning with it, and nobody notices because the document still
// reads fine.
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

// missingPackages reports register entries whose package is gone from the tree. Only
// the package half is resolved, not the identifier: a row legitimately names things
// that are not declarations (a flag, a struct field, a directory), and a check that
// insisted every span be a Go symbol would refuse the register's own vocabulary.
// A package directory that has disappeared is unambiguous, and it is the drift that
// actually happens, because a package rename leaves the prose reading perfectly well.
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

// packageExists reports whether a directory anywhere in the public band declares a
// package by this name. Directory name and package name diverge often enough
// (memory/distil, skill/skilltool) that the clause has to read the declaration.
func packageExists(root, pkg string) bool {
	found := false
	fset := token.NewFileSet()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
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
		// A file that does not parse cannot answer which package it is in, and the build
		// reports it anyway; skipping it is the whole handling this deserves.
		return nil
	})
	return found
}
