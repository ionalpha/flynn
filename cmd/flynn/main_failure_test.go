package main

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/notices"
	"github.com/ionalpha/flynn/provider"
)

// unopenableDataDir returns a data directory path no store can be opened under, because a
// regular file sits where a directory would have to be. Creating the directory fails on
// every platform, which is what makes the commands below report a failure rather than
// quietly work against a store somewhere else.
func unopenableDataDir(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	return filepath.Join(blocker, "data")
}

// keepRunGlobals puts back, when t ends, the package variables [run] assigns from its
// flags. They outlive the call, so without this one test's flags decide what a later
// test sees, and only some shuffle orders show it. TestRunWritesOnlyTheGlobalsItRestores
// fails when run starts assigning a variable this does not list.
func keepRunGlobals(t *testing.T) {
	t.Helper()
	model, project, sel, bundled := modelSpecExplicit, projectFilesFlag, selectionConfig, bundledSkillsDisabled
	t.Cleanup(func() {
		modelSpecExplicit, projectFilesFlag, selectionConfig, bundledSkillsDisabled = model, project, sel, bundled
	})
}

// restoredRunGlobals is the set [keepRunGlobals] puts back.
var restoredRunGlobals = []string{"bundledSkillsDisabled", "modelSpecExplicit", "projectFilesFlag", "selectionConfig"}

// TestRunWritesOnlyTheGlobalsItRestores reads run's source and fails on an assignment to
// a package variable that [keepRunGlobals] does not restore, so a new global flag cannot
// leak between tests unnoticed.
func TestRunWritesOnlyTheGlobalsItRestores(t *testing.T) {
	fset := token.NewFileSet()
	pkgVars := map[string]bool{}
	var runFn *ast.FuncDecl
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					for _, id := range s.(*ast.ValueSpec).Names {
						pkgVars[id.Name] = true
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "run" {
					runFn = d
				}
			}
		}
	}
	if runFn == nil {
		t.Fatal("func run not found")
	}
	written := map[string]bool{}
	ast.Inspect(runFn.Body, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok && a.Tok == token.ASSIGN {
			for _, lhs := range a.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" || !pkgVars[id.Name] {
					continue
				}
				// A name declared inside run shadows the package variable.
				local := id.Obj != nil && id.Obj.Pos() >= runFn.Pos() && id.Obj.Pos() < runFn.End()
				if !local {
					written[id.Name] = true
				}
			}
		}
		return true
	})
	var got []string
	for name := range written {
		got = append(got, name)
	}
	sort.Strings(got)
	if !slices.Equal(got, restoredRunGlobals) {
		t.Fatalf("run assigns package variables %v; keepRunGlobals restores %v", got, restoredRunGlobals)
	}
}

// runCLIIn drives run the way runCLI does, but against a data directory the caller chooses,
// so the failure half of each dispatch can be reached. A command that cannot open its store
// has to say so and exit non-zero: exiting 0 on a store it never opened would report an
// empty history as though it were the truth.
func runCLIIn(t *testing.T, dataDir string, args ...string) runResult {
	t.Helper()
	t.Setenv(notices.OffEnv, "1")
	for _, k := range provider.CredentialEnvVars() {
		t.Setenv(k, "")
	}
	t.Setenv(diag.EnvDir, "")

	keepRunGlobals(t)

	var out, errs bytes.Buffer
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(&errs)
	full := append([]string{"--data-dir", dataDir}, args...)
	code := run(fs, full, &out, &errs)
	return runResult{code: code, stdout: out.String(), stderr: errs.String()}
}

// TestRunReportsAStoreItCannotOpen walks the dispatches that read the store and checks each
// one fails loudly. The distinction being pinned is between "there is nothing recorded" and
// "the recording could not be read": the first is exit 0 and an empty listing, the second has
// to be exit 1 with the reason on stderr, or a broken data directory reads as a clean history.
func TestRunReportsAStoreItCannotOpen(t *testing.T) {
	for _, cmd := range [][]string{
		{"runs"},
		{"sessions"},
		{"regrade"},
		{"inspect", "some-run"},
		{"get", "instances"},
		{"describe", "instances", "one"},
		{"diff", "instances", "one", "two"},
		{"watch"},
		{"deps", "check"},
		{"integrations", "show", "httpbin"},
		{"integrations", "call", "httpbin", "get"},
		{"services", "rm", "svc"},
		{"playbook", "run", "fly-app"},
		{"skill", "ls"},
		{"skill", "show", "deletion"},
	} {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			got := runCLIIn(t, unopenableDataDir(t), cmd...)
			if got.code != 1 {
				t.Fatalf("exit = %d, want 1 (stdout %q, stderr %q)", got.code, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stderr, "error:") {
				t.Fatalf("stderr = %q, want the failure reported", got.stderr)
			}
		})
	}
}
