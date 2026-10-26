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

// rootName is how the prose names the module root, which dev/apidiff writes as ".".
// The tier is written in the Stability tiers section of ARCHITECTURE.md and the
// tier table of docs/VERSIONING.md, which are promises, and in the STABLE list of
// dev/apidiff, which is the gate that keeps them. A name missing from the gate is a
// promise nothing enforces, which is how the root agent package went unchecked
// while ARCHITECTURE.md called it stable. The tests in this file fail when those
// three disagree, so the next edit to one meets the others.
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

// backtickedNames returns the code spans of a markdown line, in order.
func backtickedNames(line string) []string {
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
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
	return backtickedNames(body)
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
	vers := backtickedNames(row)
	_, domainRow, ok := strings.Cut(versioning, "| Domain surface |")
	if !ok {
		t.Fatal("docs/VERSIONING.md has no Domain surface row in its tier table")
	}
	domainRow, _, _ = strings.Cut(domainRow, "\n")
	versDomain := backtickedNames(domainRow)

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

	// Both domain lists end in "and the rest", so neither is complete; what they
	// name must agree with each other and must not be a gated package.
	domains := []struct {
		name string
		got  []string
	}{
		{"ARCHITECTURE.md domain surface", archDomain},
		{"docs/VERSIONING.md domain row", versDomain},
	}
	for _, l := range domains {
		if len(l.got) < 5 {
			t.Errorf("%s: read %d domain names; the parser has lost the list", l.name, len(l.got))
		}
		if gated := onlyIn(l.got, onlyIn(l.got, gate)); len(gated) > 0 {
			t.Errorf("%s calls %v unstable but dev/apidiff gates it as stable", l.name, gated)
		}
		for _, name := range l.got {
			if !hasShippedGo(filepath.Join("..", "..", filepath.FromSlash(name))) {
				t.Errorf("%s names %q, which is not a package of the module", l.name, name)
			}
		}
	}
	if d := onlyIn(archDomain, versDomain); len(d) > 0 {
		t.Errorf("ARCHITECTURE.md calls %v unstable and docs/VERSIONING.md does not name them", d)
	}
	if d := onlyIn(versDomain, archDomain); len(d) > 0 {
		t.Errorf("docs/VERSIONING.md calls %v unstable and ARCHITECTURE.md does not name them", d)
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
// concurrency-control state, with the suite a backend must pass.
type securityInterface struct {
	pkg, name string
	suite     string
	carries   string
}

// securityInterfaces is the manifest TestEveryStableSecurityInterfaceHasSuite
// reads. An interface joins it when an implementation that gets its contract
// wrong would let a record be rewritten, a write be lost, or a claim be held
// twice. dispatch.Admitter is the fail-closed gate every action passes, but it is
// not stable and holds no state, so it is not listed; it joins both when it has a
// contract suite.
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

// exposedImports adds to out the import path of every package that n names in a
// way a caller of the declaration can see. Unexported struct fields and interface
// methods are skipped: a host cannot name or implement them, so a package they
// mention is not part of the promise.
func exposedImports(n ast.Node, imports map[string]string, out map[string]bool) {
	visible := func(f *ast.Field) bool {
		return len(f.Names) == 0 || slices.ContainsFunc(f.Names, func(id *ast.Ident) bool { return id.IsExported() })
	}
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.StructType:
			for _, f := range x.Fields.List {
				if visible(f) {
					exposedImports(f.Type, imports, out)
				}
			}
			return false
		case *ast.InterfaceType:
			for _, f := range x.Methods.List {
				if visible(f) {
					exposedImports(f.Type, imports, out)
				}
			}
			return false
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				if p, ok := imports[id.Name]; ok {
					out[p] = true
				}
			}
		}
		return true
	})
}

// exportedAPIImports returns the import paths of the packages the exported
// declarations of the shipped files in dir mention in their types.
func exportedAPIImports(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	forShippedFiles(t, dir, func(f *ast.File) {
		imports := map[string]string{}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			name := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imports[name] = path
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv != nil {
					if !slices.ContainsFunc(d.Recv.List, func(r *ast.Field) bool { return exportedReceiver(r.Type) }) {
						continue
					}
					exposedImports(d.Recv, imports, out)
				}
				exposedImports(d.Type, imports, out)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exposedImports(s.Type, imports, out)
						}
					case *ast.ValueSpec:
						if s.Type != nil && slices.ContainsFunc(s.Names, func(id *ast.Ident) bool { return id.IsExported() }) {
							exposedImports(s.Type, imports, out)
						}
					}
				}
			}
		}
	})
	return out
}

// exportedReceiver reports whether a method receiver type names an exported type.
func exportedReceiver(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.StarExpr:
		return exportedReceiver(x.X)
	case *ast.IndexExpr:
		return exportedReceiver(x.X)
	case *ast.Ident:
		return x.IsExported()
	}
	return false
}

// ungatedExposures are the places a stable package names a package outside the
// stable tier in its exported API, each with why it is tolerated for now. An
// entry is a debt: a break in the unstable package changes the stable shape and
// dev/apidiff reports it as domain, not as the break it is.
var ungatedExposures = map[string]string{
	"agent -> archetypes":     "agent.Config.Agent is an *archetypes.Spec, the input of the governed archetype run, which is still being shaped. Promote archetypes when it settles, or replace the field with a type the facade owns.",
	"agent -> governed":       "agent.Config carries governed.ToolBridge, *governed.ExternalAgent and []governed.Waiver, and Agent.GoalResult returns a governed.Result, so a host configures and reads the facade's run in the assembly's own terms. governed is new and its options are still settling; promote it with a suite for what a governed run must record, or give the facade its own types.",
	"agent -> mission":        "agent.Config.Prompter, Reporter and Tools are mission.ApprovalPrompter, mission.Reporter and []mission.Tool, the same contracts the assembly takes. mission is the large, moving orchestration package; this goes with tools -> mission once the tool and approval contracts move to a small stable package.",
	"sandbox -> netguard":     "sandbox.WithEgress takes a netguard.Policy. netguard is one small proxy package; promote it once its Policy is settled.",
	"capability -> dispatch":  "capability.Admitter and ContainmentGate implement dispatch.Admitter and dispatch.Hook, so dispatch.Action and dispatch.Metering are in their signatures. dispatch is unstable until a suite states what a correct admitter does; promote it with that suite.",
	"tools -> mission":        "tools hands the model its tools as mission.Tool values and mission.ResultSummarizer. mission is the large, moving orchestration package, so promoting it is not an option; the tool contract needs to move to a small stable package first.",
	"tools/github -> mission": "the same mission.Tool exposure as tools, through the GitHub toolset.",
}

// TestStableAPIOnlyNamesGatedTypes fails when an exported declaration of a stable
// package names a type from a module package outside the stable tier. A type that
// appears in the signature or fields of the stable surface is part of it whether
// or not anyone promised it, and a break in an ungated package would pass
// dev/apidiff as a domain change while changing what the stable surface means.
// Tolerated exceptions are in [ungatedExposures], with the reason.
func TestStableAPIOnlyNamesGatedTypes(t *testing.T) {
	stable, _ := apidiffLists(t)
	module := modulePath(t, filepath.Join("..", ".."))
	seen := map[string]bool{}
	for _, name := range stable {
		root := filepath.Join("..", "..", filepath.FromSlash(name))
		if name == rootName {
			root = filepath.Join("..", "..")
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(filepath.Join("..", ".."), path)
			rel = filepath.ToSlash(rel)
			if d.Name() == "testdata" || d.Name() == "internal" || (name == rootName && rel != ".") {
				return filepath.SkipDir
			}
			from := rel
			if rel == "." {
				from = rootName
			}
			for imp := range exportedAPIImports(t, path) {
				target, ok := strings.CutPrefix(imp, module+"/")
				if !ok || strings.HasPrefix(target, "internal/") || underStable(stable, target) {
					continue
				}
				key := from + " -> " + target
				seen[key] = true
				if _, tolerated := ungatedExposures[key]; !tolerated {
					t.Errorf("%s names a type from %s in its exported API, and %s is not in the stable tier: gate it, or stop exposing it", from, target, target)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", name, err)
		}
	}
	for key := range ungatedExposures {
		if !seen[key] {
			t.Errorf("ungatedExposures lists %q, which no longer holds: delete the entry", key)
		}
	}
}
