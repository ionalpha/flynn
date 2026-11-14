package rigor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// policyDirective marks a type that decides who or what is trusted. The line goes in the
// type's doc comment, optionally followed by the name of the test that proves the type
// refuses its zero value, an unknown value and an unknown name; without a name the test is
// TestFailClosed followed by the type's name. A fail-open check admits whatever it did not
// anticipate, and the bugs of that kind in this repository were each one type nobody had
// asked "what does the zero value do?" of, so the question is made a gate: a marked type
// with no such test fails the build.
const policyDirective = "//flynn:policy"

// policyNameSuffixes are the endings that make a type look like a trust decision. An
// unmarked type ending in one must either carry the directive or be listed in
// failClosedExemptions, so a new policy type cannot slip in without the question being
// asked of it.
var policyNameSuffixes = []string{"policy", "guard", "verifier", "authenticator", "admitter", "gate", "exposure", "allowlist"}

// failClosedExemptions names the policy-looking types that carry no directive, keyed by
// package directory and type name, each with the reason. Two kinds are listed: types that
// decide nothing about trust, and types that do and still need their test. The second kind
// only shrinks: a test is added, the directive goes on, and the entry is removed, which the
// stale-entry check below enforces.
var failClosedExemptions = map[string]string{
	"(root).Policy":               "per-run configuration lists; the trust decisions it feeds are checked in governed and approval. Fail-closed test owed",
	"inbox.Policy":                "intake triage policy; fail-closed test owed",
	"cmd/flynn.consentPolicy":     "CLI consent mode; fail-closed test owed",
	"cmd/flynn.senderAllowList":   "inbound sender allow-list; fail-closed test owed",
	"cmd/flynn.senderGate":        "inbound sender gate; fail-closed test owed",
	"capability.Admitter":         "capability grant check; fail-closed test owed",
	"capability.ContainmentGate":  "containment gate; fail-closed test owed",
	"dispatch.Admitter":           "interface contract only; each implementation is checked where it is declared",
	"governed.boundAdmitter":      "wraps a capability.Admitter and adds nothing to the decision",
	"goal.ItemVerifier":           "goal item verification contract; fail-closed test owed",
	"goal.EvidenceGate":           "evidence gate for goal completion; fail-closed test owed",
	"learn.Verifier":              "verification contract for learned items; fail-closed test owed",
	"learn.SandboxVerifier":       "sandboxed verification of learned items; fail-closed test owed",
	"learn.GovernedVerifier":      "governed verification of learned items; fail-closed test owed",
	"learn.DecayPolicy":           "tunes how fast evidence ages; decides no access",
	"chain.Verifier":              "provenance chain verification; fail-closed test owed",
	"chain.ed25519CoseVerifier":   "signature check behind chain.Verifier; fail-closed test owed",
	"evidence.CommandVerifier":    "runs a verification command; fail-closed test owed",
	"internal/sigstore.Verifier":  "release signature verification; fail-closed test owed",
	"internal/release.policy":     "fixed identity a release signature must present; it is not configurable",
	"internal/projectctx.Policy":  "project instruction file loading; fail-closed test owed",
	"internal/hostneutral.Policy": "host-neutrality rules for the scanner; fail-closed test owed",
	"internal/rigor.Policy":       "which packages the rigor scan covers; decides no access",
	"sandbox.floorPolicy":         "counts opt-in uses of the process-jail floor; the opt-in defaults to off and is covered with the sandbox types",
	"tools/github.authenticator":  "holds the GitHub App credentials for token minting; decides no access",
}

// typeInfo is one type declaration found in a non-test file.
type typeInfo struct {
	name   string
	marked bool
	// test is the test name the directive asks for, or "" for the default.
	test string
}

// fileScan is what the gate reads from one file.
type fileScan struct {
	types []typeInfo
	tests []string
}

// scanFile parses src as filename and returns its type declarations and top-level test
// functions.
func scanFile(t *testing.T, filename string, src any) fileScan {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var out fileScan
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && strings.HasPrefix(d.Name.Name, "Test") {
				out.tests = append(out.tests, d.Name.Name)
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts := spec.(*ast.TypeSpec)
				marked, test := directive(d.Doc)
				if m, tn := directive(ts.Doc); m {
					marked, test = m, tn
				}
				out.types = append(out.types, typeInfo{name: ts.Name.Name, marked: marked, test: test})
			}
		}
	}
	return out
}

// directive reports whether a doc comment carries the policy directive, and the test name
// it names.
func directive(g *ast.CommentGroup) (marked bool, test string) {
	if g == nil {
		return false, ""
	}
	for _, c := range g.List {
		rest, ok := strings.CutPrefix(c.Text, policyDirective)
		if !ok || (rest != "" && rest[0] != ' ') {
			continue
		}
		return true, strings.TrimSpace(rest)
	}
	return false, ""
}

// looksLikePolicy reports whether a type name ends in one of policyNameSuffixes.
func looksLikePolicy(name string) bool {
	l := strings.ToLower(name)
	for _, s := range policyNameSuffixes {
		if strings.HasSuffix(l, s) {
			return true
		}
	}
	return false
}

// pkgScan is one directory: its non-test types and the tests of all its test files.
type pkgScan struct {
	types []typeInfo
	tests map[string]bool
}

// failClosedFindings applies the gate to the scanned packages, keyed by directory relative
// to the module root. Nothing here touches the file system, so the fixtures below run it on
// synthetic packages.
func failClosedFindings(pkgs map[string]pkgScan, exemptions map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for dir, p := range pkgs {
		for _, ty := range p.types {
			key := dir + "." + ty.name
			seen[key] = true
			if ty.marked {
				want := ty.test
				if want == "" {
					want = "TestFailClosed" + ty.name
				}
				if !p.tests[want] {
					out = append(out, key+" is marked "+policyDirective+" but its package has no "+want+": add a test that the zero value, an unknown value and an unknown name are refused")
				}
				if _, ex := exemptions[key]; ex {
					out = append(out, key+" is marked and also listed in failClosedExemptions: remove the entry")
				}
				continue
			}
			if !looksLikePolicy(ty.name) {
				continue
			}
			if reason, ex := exemptions[key]; !ex {
				out = append(out, key+" looks like a trust decision but has no "+policyDirective+" directive and no entry in failClosedExemptions: write its TestFailClosed test and mark it, or list it with the reason it decides no access")
			} else if reason == "" {
				out = append(out, key+" is exempt without a reason")
			}
		}
	}
	for key := range exemptions {
		if !seen[key] {
			out = append(out, key+" is in failClosedExemptions but no such type exists: remove the entry")
		}
	}
	sort.Strings(out)
	return out
}

// scanModule reads every non-generated Go file under root into per-directory scans.
func scanModule(t *testing.T, root string) map[string]pkgScan {
	t.Helper()
	pkgs := map[string]pkgScan{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(rel)
		if dir == "." {
			dir = "(root)"
		}
		p := pkgs[dir]
		if p.tests == nil {
			p.tests = map[string]bool{}
		}
		sc := scanFile(t, path, nil)
		if strings.HasSuffix(name, "_test.go") {
			for _, tn := range sc.tests {
				p.tests[tn] = true
			}
		} else {
			p.types = append(p.types, sc.types...)
		}
		pkgs[dir] = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

// TestPolicyTypesHaveFailClosedTests fails when a type marked //flynn:policy has no
// fail-closed test in its package, when a policy-looking type is neither marked nor
// exempted with a reason, and when an exemption names a type that is gone.
func TestPolicyTypesHaveFailClosedTests(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}
	for _, f := range failClosedFindings(scanModule(t, root), failClosedExemptions) {
		t.Error(f)
	}
}

// TestFailClosedGateFailsOnABareOrUntestedPolicy keeps the gate honest: a marked type
// without its test, an unmarked policy-looking type, an exemption with no reason and a
// stale exemption are each reported, and a marked type with its test, a named test and a
// justified exemption are not.
func TestFailClosedGateFailsOnABareOrUntestedPolicy(t *testing.T) {
	src := `package p

//flynn:policy
type Trust int

//flynn:policy TestFailClosedAll
type Scope int

// Untested is marked but has no test.
//
//flynn:policy
type Untested struct{}

type Bare struct{}

type ExposurePolicy struct{}

type LooseGuard struct{}

type Plain struct{}

type (
	//flynn:policy
	Grouped int

	GroupVerifier int
)

//flynn:policyish
type NotADirective int

type Listed struct{}
`
	tests := scanFile(t, "p_test.go", `package p
import "testing"
func TestFailClosedTrust(t *testing.T) {}
func TestFailClosedAll(t *testing.T) {}
func TestFailClosedGrouped(t *testing.T) {}
func (x X) TestFailClosedUntested() {}
`)
	if len(tests.tests) != 3 {
		t.Fatalf("a method counted as a test: %v", tests.tests)
	}
	p := pkgScan{types: scanFile(t, "p.go", src).types, tests: map[string]bool{}}
	for _, tn := range tests.tests {
		p.tests[tn] = true
	}
	exempt := map[string]string{
		"p.LooseGuard":  "",
		"p.Gone":        "was removed",
		"p.Trust":       "marked and exempt at once",
		"p.ListedGuard": "never declared",
	}
	got := strings.Join(failClosedFindings(map[string]pkgScan{"p": p}, exempt), "\n")
	for _, want := range []string{
		"p.Untested is marked",
		"p.ExposurePolicy looks like a trust decision",
		"p.GroupVerifier looks like a trust decision",
		"p.LooseGuard is exempt without a reason",
		"p.Gone is in failClosedExemptions but no such type exists",
		"p.Trust is marked and also listed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the gate did not report %q; got:\n%s", want, got)
		}
	}
	for _, clean := range []string{"p.Scope", "p.Grouped", "p.Bare", "p.Plain", "p.NotADirective", "p.Listed "} {
		if strings.Contains(got, clean+" ") || strings.Contains(got, clean+"\n") {
			t.Errorf("the gate reported %q, which is fine; got:\n%s", clean, got)
		}
	}
}
