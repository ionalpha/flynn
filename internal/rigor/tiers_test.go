package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The stable tier is written down in three places a person reads and once more in
// the one a machine acts on: the Stability tiers section of ARCHITECTURE.md, the
// tier table of docs/VERSIONING.md, and the STABLE list of dev/apidiff. The first
// two are promises; the last is the gate that keeps them. A name missing from the
// gate is a promise nothing enforces, which is how the root agent package went
// unchecked while ARCHITECTURE.md called it stable. These tests fail when the four
// disagree, so the next edit to any of them meets the others.

// rootName is how the prose names the module root, which dev/apidiff writes as ".".
const rootName = "agent"

var (
	backticked   = regexp.MustCompile("`([^`]+)`")
	apidiffList  = regexp.MustCompile(`(?m)^(STABLE|SUITES)="([^"]*)"`)
	acceptedLine = regexp.MustCompile(`^- `)
)

// readRepoFile reads a file named relative to the repository root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// apidiffLists returns the STABLE and SUITES lists of dev/apidiff, with the root
// entry renamed to what the prose calls it.
func apidiffLists(t *testing.T) (stable, suites []string) {
	t.Helper()
	src := readRepoFile(t, "dev/apidiff")
	for _, m := range apidiffList.FindAllStringSubmatch(src, -1) {
		fields := strings.Fields(m[2])
		if m[1] == "SUITES" {
			suites = fields
			continue
		}
		for _, f := range fields {
			if f == "." {
				f = rootName
			}
			stable = append(stable, f)
		}
	}
	if len(stable) == 0 || len(suites) == 0 {
		t.Fatalf("dev/apidiff: found STABLE (%d entries) and SUITES (%d entries); both must be set", len(stable), len(suites))
	}
	return stable, suites
}

// bulletTokens returns the backticked names in the markdown text from the line
// containing start up to, not including, the line containing end.
func bulletTokens(t *testing.T, doc, start, end string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, start)
	if !ok {
		t.Fatalf("no text %q", start)
	}
	body, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatalf("no text %q after %q", end, start)
	}
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// onlyIn returns the entries of a that are not in b, sorted and without repeats.
func onlyIn(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

func TestOnlyIn(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{"equal", []string{"a", "b"}, []string{"b", "a"}, nil},
		{"extra in a", []string{"a", "b", "c"}, []string{"a"}, []string{"b", "c"}},
		{"nothing in a", nil, []string{"a"}, nil},
		{"repeats collapse", []string{"x", "x"}, nil, []string{"x"}},
	}
	for _, c := range cases {
		if got := onlyIn(c.a, c.b); !slices.Equal(got, c.want) {
			t.Errorf("%s: onlyIn(%v, %v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

// TestStableTierListsAgree fails when ARCHITECTURE.md, docs/VERSIONING.md and the
// STABLE list of dev/apidiff name different stable packages, when a name in them
// is not a package of the module, or when a package is promised both stable and
// unstable.
func TestStableTierListsAgree(t *testing.T) {
	gate, _ := apidiffLists(t)

	arch := bulletTokens(t, readRepoFile(t, "ARCHITECTURE.md"), "- **Stable surface**", "- **Domain surface**")
	archDomain := bulletTokens(t, readRepoFile(t, "ARCHITECTURE.md"), "- **Domain surface**", "- **Mechanisms**")

	versioning := readRepoFile(t, "docs/VERSIONING.md")
	_, row, ok := strings.Cut(versioning, "| Stable surface |")
	if !ok {
		t.Fatal("docs/VERSIONING.md has no Stable surface row in its tier table")
	}
	row, _, _ = strings.Cut(row, "\n")
	var vers []string
	for _, m := range backticked.FindAllStringSubmatch(row, -1) {
		vers = append(vers, m[1])
	}

	lists := []struct {
		name string
		got  []string
	}{
		{"ARCHITECTURE.md Stability tiers", arch},
		{"docs/VERSIONING.md tier table", vers},
	}
	for _, l := range lists {
		if len(l.got) < 10 {
			t.Errorf("%s: read %d stable names; the parser has lost the list", l.name, len(l.got))
		}
		if missing := onlyIn(gate, l.got); len(missing) > 0 {
			t.Errorf("%s omits %v, which dev/apidiff gates as stable: a package that is gated is promised", l.name, missing)
		}
		if extra := onlyIn(l.got, gate); len(extra) > 0 {
			t.Errorf("%s promises %v as stable but dev/apidiff does not gate it: add it to STABLE or stop promising it", l.name, extra)
		}
	}
	if both := onlyIn(arch, onlyIn(arch, archDomain)); len(both) > 0 {
		t.Errorf("ARCHITECTURE.md lists %v as both stable and domain", both)
	}

	for _, name := range gate {
		if name == rootName {
			continue
		}
		if !hasShippedGo(filepath.Join("..", "..", filepath.FromSlash(name))) {
			t.Errorf("stable name %q is not a package of the module (no shipped Go files at %s)", name, name)
		}
	}
}

// hasShippedGo reports whether dir holds a non-test Go file.
func hasShippedGo(dir string) bool {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(files, func(f string) bool { return !strings.HasSuffix(f, "_test.go") })
}

// underStable reports whether a package path is covered by a name of the STABLE
// list, which dev/apidiff matches as a path prefix.
func underStable(stable []string, pkg string) bool {
	return slices.ContainsFunc(stable, func(s string) bool { return pkg == s || strings.HasPrefix(pkg, s+"/") })
}

// TestConformanceSuitesAreNamedAndGated fails when a conformance suite that
// dev/apidiff watches is missing from the compatibility promise in
// docs/VERSIONING.md, is not itself under a stable path (so its exported
// signature would be ungated), or is not a package.
func TestConformanceSuitesAreNamedAndGated(t *testing.T) {
	stable, suites := apidiffLists(t)
	versioning := readRepoFile(t, "docs/VERSIONING.md")
	for _, s := range suites {
		if !hasShippedGo(filepath.Join("..", "..", filepath.FromSlash(s))) {
			t.Errorf("suite %q has no shipped Go files", s)
		}
		if !underStable(stable, s) {
			t.Errorf("suite %q is not under a stable path in dev/apidiff: the port it checks is promised and its entry point is not", s)
		}
		if !strings.Contains(versioning, "`"+s+"`") {
			t.Errorf("docs/VERSIONING.md does not name suite %q in the compatibility promise", s)
		}
	}
}

// securityInterface is a stable interface that carries audit, fencing or
// concurrency-control state, with the suite a backend must pass. The security
// property each suite checks is stated in its package comment.
type securityInterface struct {
	pkg, name string
	suite     string
	carries   string
}

// securityInterfaces is the manifest TestEveryStableSecurityInterfaceHasSuite
// reads. An interface joins it when an implementation that gets its contract
// wrong would let a record be rewritten, a write be lost, or a claim be held
// twice. dispatch.Admitter and dispatch.Hook are not listed: they hold no state of
// their own, so no backend exists to certify; what they decide is tested at the
// waist, per policy.
var securityInterfaces = []securityInterface{
	{"spine", "Log", "spine/spinetest", "the audit log: append-only order and sequence"},
	{"resource", "Store", "resource/resourcetest", "versioned writes and scope isolation"},
	{"jobs", "Queue", "jobs/jobstest", "leases: a ready job is claimed by one worker at a time"},
	{"state", "MemoryStore", "memory/memorytest", "recorded provenance, promotion and usage of memory"},
}

// TestEveryStableSecurityInterfaceHasSuite fails when an interface in the manifest
// is not declared in a stable package, when its suite is not watched by
// dev/apidiff, or when the suite does not import the package that declares the
// interface (a suite that never touches the port certifies nothing about it).
func TestEveryStableSecurityInterfaceHasSuite(t *testing.T) {
	stable, suites := apidiffLists(t)
	module := modulePath(t, filepath.Join("..", ".."))
	for _, si := range securityInterfaces {
		who := si.pkg + "." + si.name
		if si.carries == "" {
			t.Errorf("%s: the manifest entry says nothing about what it carries", who)
		}
		if !underStable(stable, si.pkg) {
			t.Errorf("%s: package %s is not stable, so the interface is not in the promise", who, si.pkg)
		}
		if !declaresInterface(t, filepath.Join("..", "..", filepath.FromSlash(si.pkg)), si.name) {
			t.Errorf("%s: no interface of that name is declared in %s", who, si.pkg)
		}
		if !slices.Contains(suites, si.suite) {
			t.Errorf("%s: suite %s is not in dev/apidiff SUITES", who, si.suite)
		}
		if !importsPackage(t, filepath.Join("..", "..", filepath.FromSlash(si.suite)), module+"/"+si.pkg) {
			t.Errorf("%s: suite %s does not import %s", who, si.suite, si.pkg)
		}
	}
}

// declaresInterface reports whether a shipped file of dir declares interface name.
func declaresInterface(t *testing.T, dir, name string) bool {
	t.Helper()
	found := false
	forShippedFiles(t, dir, func(f *ast.File) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					continue
				}
				if _, ok := ts.Type.(*ast.InterfaceType); ok {
					found = true
				}
			}
		}
	})
	return found
}

// importsPackage reports whether a shipped file of dir imports path.
func importsPackage(t *testing.T, dir, path string) bool {
	t.Helper()
	found := false
	forShippedFiles(t, dir, func(f *ast.File) {
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == path {
				found = true
			}
		}
	})
	return found
}

func forShippedFiles(t *testing.T, dir string, fn func(*ast.File)) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		fn(f)
	}
}

// unreasoned returns the accepted-break entries in text that are not argued for.
// An entry is argued for when a comment line with words in it sits in the same
// blank-line-separated block, above the entry.
func unreasoned(text string) []string {
	var bad []string
	reasoned := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			reasoned = false
		case strings.HasPrefix(line, "#"):
			if strings.TrimSpace(strings.TrimPrefix(line, "#")) != "" && !strings.HasPrefix(line, "# base:") {
				reasoned = true
			}
		case acceptedLine.MatchString(line):
			if !reasoned {
				bad = append(bad, line)
			}
		}
	}
	return bad
}

func TestUnreasoned(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		{"argued", "# why\n- ./a.B: removed\n- ./a.C: removed\n", nil},
		{"no comment", "- ./a.B: removed\n", []string{"- ./a.B: removed"}},
		{"empty comment", "#\n- ./a.B: removed\n", []string{"- ./a.B: removed"}},
		{"base header is not a reason", "# base: v1.0.0\n- ./a.B: removed\n", []string{"- ./a.B: removed"}},
		{"reason does not carry past a blank line", "# why\n- ./a.B: removed\n\n- ./a.C: removed\n", []string{"- ./a.C: removed"}},
		{"entry above its comment", "- ./a.B: removed\n# why\n", []string{"- ./a.B: removed"}},
	}
	for _, c := range cases {
		if got := unreasoned(c.text); !slices.Equal(got, c.want) {
			t.Errorf("%s: unreasoned = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestApidiffAcceptedEntriesHaveReasons fails when an entry of
// dev/apidiff-accepted.txt has no comment above it. The file is the written
// argument for each break of the stable surface; an entry without one is a
// waiver nobody can review.
func TestApidiffAcceptedEntriesHaveReasons(t *testing.T) {
	text := readRepoFile(t, "dev/apidiff-accepted.txt")
	if !regexp.MustCompile(`(?m)^# base: *\S+`).MatchString(text) {
		t.Error("dev/apidiff-accepted.txt names no base tag; dev/apidiff would ignore every entry")
	}
	for _, line := range unreasoned(text) {
		t.Errorf("accepted break has no reason above it: %s", line)
	}
}
