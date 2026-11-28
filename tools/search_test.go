package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/sandbox"
)

// listOnly hides everything but the Sandbox port, as a backend without
// directory reads would.
type listOnly struct{ sandbox.Sandbox }

// backends runs a case on a directory-reading sandbox and on one that can only
// list a whole tree.
func backends(t *testing.T, files map[string]string, run func(t *testing.T, s *Set)) {
	t.Helper()
	for _, kind := range []string{"local", "list-only"} {
		t.Run(kind, func(t *testing.T) {
			_, sb := newSet(t)
			for p, content := range files {
				seed(t, sb, p, content)
			}
			if kind == "list-only" {
				run(t, New(listOnly{sb}))
				return
			}
			run(t, New(sb))
		})
	}
}

// ignoreTree holds "needle" in every file, so any file a search reaches shows.
func ignoreTree() map[string]string {
	files := map[string]string{
		".gitignore":            "node_modules/\n*.log\n/dist\n!keep.log\nsecret[0-9].txt\n",
		".ignore":               "fixtures/\n",
		".git/info/exclude":     "local-only.txt\n",
		"src/.gitignore":        "gen/\n!special.log\n",
		"main.go":               "needle",
		"keep.log":              "needle",
		"debug.log":             "needle",
		"secret1.txt":           "needle",
		"local-only.txt":        "needle",
		"dist/out.js":           "needle",
		"fixtures/f.txt":        "needle",
		"node_modules/m/i.js":   "needle",
		"src/a.go":              "needle",
		"src/dist/kept.js":      "needle",
		"src/gen/g.go":          "needle",
		"src/special.log":       "needle",
		"src/deep/er/est.go":    "needle",
		"src/node_modules/x.js": "needle",
		".git/config":           "needle",
		"sub/.git/HEAD":         "needle",
		".hg/store":             "needle",
		".svn/entries":          "needle",
		".jj/repo":              "needle",
		".github/ci.yml":        "needle",
	}
	return files
}

// visible is what a search of ignoreTree reaches, worked out by hand from the
// gitignore rules above.
var visible = []string{
	".github/ci.yml",
	"keep.log",
	"main.go",
	"src/a.go",
	"src/deep/er/est.go",
	"src/dist/kept.js",
	"src/special.log",
}

func resultLines(out string) []string {
	lines := strings.Split(out, "\n")
	sort.Strings(lines)
	return lines
}

func TestSearchSkipsIgnoredAndVCS(t *testing.T) {
	backends(t, ignoreTree(), func(t *testing.T, s *Set) {
		got, err := invoke(t, grepTool{s}, map[string]any{"pattern": "needle"})
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, f := range visible {
			want = append(want, f+":1:needle")
		}
		if g := strings.Join(resultLines(got), "\n"); g != strings.Join(want, "\n") {
			t.Errorf("grep reached:\n%s\nwant:\n%s", g, strings.Join(want, "\n"))
		}

		got, err = invoke(t, globTool{s}, map[string]any{"pattern": "**/*.{go,js,log,txt,yml}"})
		if err != nil {
			t.Fatal(err)
		}
		if got != strings.Join(visible, "\n") {
			t.Errorf("glob listed:\n%s\nwant:\n%s", got, strings.Join(visible, "\n"))
		}
	})
}

// A path the caller names is searched even when an ignore rule covers it, as
// ripgrep does; the rules still apply below it.
func TestSearchNamedIgnoredPath(t *testing.T) {
	files := ignoreTree()
	files["dist/trace.log"] = "needle"
	backends(t, files, func(t *testing.T, s *Set) {
		got, err := invoke(t, grepTool{s}, map[string]any{"pattern": "needle", "path": "./dist/"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "dist/out.js:1:needle" {
			t.Errorf("grep dist = %q", got)
		}
		got, err = invoke(t, globTool{s}, map[string]any{"pattern": "dist/*"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "dist/out.js" {
			t.Errorf("glob dist/* = %q", got)
		}
	})
}

func TestGlobSemantics(t *testing.T) {
	files := map[string]string{
		"a.go": "", "b.txt": "", "x1.md": "", "x2.md": "",
		"cmd/main.go": "", "pkg/a/a.go": "", "pkg/a/a_test.go": "", "pkg/a/b/c/d.go": "",
		"web/app.ts": "", "web/app.tsx": "",
	}
	cases := []struct{ pattern, want string }{
		{"*.go", "a.go"},
		{"**/*.go", "a.go\ncmd/main.go\npkg/a/a.go\npkg/a/a_test.go\npkg/a/b/c/d.go"},
		{"./**/*.go", "a.go\ncmd/main.go\npkg/a/a.go\npkg/a/a_test.go\npkg/a/b/c/d.go"},
		{"pkg/**/*.go", "pkg/a/a.go\npkg/a/a_test.go\npkg/a/b/c/d.go"},
		{"pkg/**/d.go", "pkg/a/b/c/d.go"},
		{"**/b/**", "pkg/a/b\npkg/a/b/c\npkg/a/b/c/d.go"},
		{"web/*.{ts,tsx}", "web/app.ts\nweb/app.tsx"},
		{"{cmd,web}/**/*.{go,ts}", "cmd/main.go\nweb/app.ts"},
		{"{a.go,pkg/a/{a,a_test}.go}", "a.go\npkg/a/a.go\npkg/a/a_test.go"},
		{"x[!1].md", "x2.md"},
		{"x[^1].md", "x2.md"},
		{"x?.md", "x1.md\nx2.md"},
		{"**/nothing", "no matches"},
		{"missing/dir/*.go", "no matches"},
	}
	_, sb := newSet(t)
	for p, content := range files {
		seed(t, sb, p, content)
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			got, err := invoke(t, globTool{New(sb)}, map[string]any{"pattern": tc.pattern})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("glob %q =\n%s\nwant:\n%s", tc.pattern, got, tc.want)
			}
		})
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	s, sb := newSet(t)
	seed(t, sb, "a.go", "needle")
	cases := []struct {
		name  string
		tool  func() (string, error)
		wants string
	}{
		{"unclosed brace", func() (string, error) { return invoke(t, globTool{s}, map[string]any{"pattern": "*.{go,ts"}) }, "brace"},
		{"glob escape", func() (string, error) { return invoke(t, globTool{s}, map[string]any{"pattern": "../*"}) }, "denied"},
		{"grep escape", func() (string, error) {
			return invoke(t, grepTool{s}, map[string]any{"pattern": "needle", "path": ".."})
		}, "denied"},
		{"grep missing path", func() (string, error) {
			return invoke(t, grepTool{s}, map[string]any{"pattern": "needle", "path": "nope"})
		}, "nope"},
		{"grep bad file filter", func() (string, error) {
			return invoke(t, grepTool{s}, map[string]any{"pattern": "needle", "glob": "[a"})
		}, "glob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.tool()
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("result %q, error %v; want an error mentioning %q", got, err, tc.wants)
			}
		})
	}
}

func TestGrepOptions(t *testing.T) {
	files := map[string]string{
		"a.go":       "one\ntwo Needle\nthree\nfour\nfive\nsix needle\r\nseven\n",
		"b.md":       "needle\n",
		"pkg/c.go":   "needle\n",
		"pkg/c.ts":   "needle\n",
		"pkg/d/e.go": "needle\n",
	}
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{
			"ignore case",
			map[string]any{"pattern": "NEEDLE", "path": "a.go", "ignore_case": true},
			"a.go:2:two Needle\na.go:6:six needle",
		},
		{
			"basename filter",
			map[string]any{"pattern": "needle", "glob": "*.go"},
			"a.go:6:six needle\npkg/c.go:1:needle\npkg/d/e.go:1:needle",
		},
		{
			"path filter",
			map[string]any{"pattern": "needle", "glob": "pkg/*.go"},
			"pkg/c.go:1:needle",
		},
		{
			"brace filter",
			map[string]any{"pattern": "needle", "glob": "*.{md,ts}"},
			"b.md:1:needle\npkg/c.ts:1:needle",
		},
		{
			"excluding filter",
			map[string]any{"pattern": "needle", "glob": "!*.go"},
			"b.md:1:needle\npkg/c.ts:1:needle",
		},
		{
			"context",
			map[string]any{"pattern": "Needle|needle", "path": "a.go", "context": 1},
			"a.go-1-one\na.go:2:two Needle\na.go-3-three\n--\na.go-5-five\na.go:6:six needle\na.go-7-seven",
		},
		{
			"before and after",
			map[string]any{"pattern": "Needle", "path": "a.go", "before_context": 1, "after_context": 2},
			"a.go-1-one\na.go:2:two Needle\na.go-3-three\na.go-4-four",
		},
		{
			"adjacent context merges",
			map[string]any{"pattern": "Needle|needle", "path": "a.go", "context": 2},
			"a.go-1-one\na.go:2:two Needle\na.go-3-three\na.go-4-four\na.go-5-five\na.go:6:six needle\na.go-7-seven",
		},
		{
			"files with matches",
			map[string]any{"pattern": "needle", "files_with_matches": true},
			"a.go\nb.md\npkg/c.go\npkg/c.ts\npkg/d/e.go",
		},
		{
			"cap counts what it left out",
			map[string]any{"pattern": "needle", "max_results": 2},
			"a.go:6:six needle\nb.md:1:needle\n... [3 more matching lines in 3 files not shown; raise max_results or narrow the search]",
		},
		{
			"cap on files",
			map[string]any{"pattern": "needle", "files_with_matches": true, "max_results": 4},
			"a.go\nb.md\npkg/c.go\npkg/c.ts\n... [1 more files not shown; raise max_results or narrow the search]",
		},
	}
	backends(t, files, func(t *testing.T, s *Set) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := invoke(t, grepTool{s}, tc.in)
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Errorf("grep %v =\n%s\nwant:\n%s", tc.in, got, tc.want)
				}
			})
		}
	})
}

func TestGrepBoundsLinesAndFiles(t *testing.T) {
	s, sb := newSet(t)
	seed(t, sb, "long.txt", "needle"+strings.Repeat("x", 2000)+"\n")
	seed(t, sb, "huge.txt", "needle\n"+strings.Repeat("y\n", 6<<20))

	got, err := invoke(t, grepTool{s}, map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	want := "long.txt:1:needle" + strings.Repeat("x", 494) + " ... [1506 more bytes]\n" +
		"... [skipped 1 files over 10 MiB: huge.txt; name one as path to search it]"
	if got != want {
		t.Errorf("grep =\n%.700s\nwant:\n%s", got, want)
	}
	got, err = invoke(t, grepTool{s}, map[string]any{"pattern": "needle", "path": "huge.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "huge.txt:1:needle" {
		t.Errorf("grep of the named file = %.200q", got)
	}
}

func TestGlobCap(t *testing.T) {
	s, sb := newSet(t)
	for i := range 520 {
		seed(t, sb, fmt.Sprintf("f%03d.txt", i), "")
	}
	got, err := invoke(t, globTool{s}, map[string]any{"pattern": "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	const note = "... [20 more matches not shown; raise max_results or narrow the pattern]"
	if len(lines) != 501 || lines[0] != "f000.txt" || lines[499] != "f499.txt" || lines[500] != note {
		t.Errorf("default cap: %d lines, last %q", len(lines), lines[len(lines)-1])
	}
	got, err = invoke(t, globTool{s}, map[string]any{"pattern": "*.txt", "max_results": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != "f000.txt\n... [519 more matches not shown; raise max_results or narrow the pattern]" {
		t.Errorf("max_results=1: %.200q", got)
	}
}

// cancelAfter cancels the search from inside its own tenth file read.
type cancelAfter struct {
	*sandbox.Local
	cancel      context.CancelFunc
	reads       atomic.Int64
	cancelledAt atomic.Int64
}

func (c *cancelAfter) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if c.reads.Add(1) == 10 {
		c.cancelledAt.Store(clock.System{}.Now().UnixNano())
		c.cancel()
	}
	return c.Local.ReadFile(ctx, p)
}

// largeTree writes dirs*perDir files straight to disk.
func largeTree(t *testing.T, dirs, perDir int) *sandbox.Local {
	t.Helper()
	root := t.TempDir()
	body := []byte(strings.Repeat("some ordinary line of source text\n", 40))
	for d := range dirs {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", d), "inner")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for f := range perDir {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	sb, err := sandbox.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestSearchCancellationReturnsPromptly(t *testing.T) {
	local := largeTree(t, 100, 100)

	t.Run("grep cancelled mid-search", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sb := &cancelAfter{Local: local, cancel: cancel}
		_, err := grepTool{New(sb)}.Invoke(ctx, json.RawMessage(`{"pattern":"absent"}`))
		took := (clock.System{}).Now().Sub(time.Unix(0, sb.cancelledAt.Load()))
		t.Logf("returned %s after cancel, %d files read", took, sb.reads.Load())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if n := sb.reads.Load(); n != 10 {
			t.Errorf("files read = %d, want 10: nothing is read after the cancel", n)
		}
		if took > 2*time.Second {
			t.Errorf("returned %s after cancel, want under 2s", took)
		}
	})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() error{
		"glob": func() error {
			_, err := globTool{New(local)}.Invoke(cancelled, json.RawMessage(`{"pattern":"**/*.txt"}`))
			return err
		},
		"walk": func() error {
			_, err := local.Walk(cancelled, ".")
			return err
		},
	} {
		t.Run(name+" already cancelled", func(t *testing.T) {
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
		})
	}
}
