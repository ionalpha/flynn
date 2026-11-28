package rigor_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ruleguardModeEnv set to "require" makes a missing golangci-lint a failure.
// dev/lint and the CI lint job set it, so the fixtures cannot be skipped there.
const ruleguardModeEnv = "FLYNN_LINT_RULES"

// ruleguardFixtures are the example packages, relative to the module root. The
// go tool leaves testdata out of "./...", so each is named.
var ruleguardFixtures = []string{
	"internal/rigor/testdata/ruleguard/state",
	"internal/rigor/testdata/ruleguard/other",
}

var wantComment = regexp.MustCompile(`// want "([^"]+)"`)

type lintLine struct {
	file string
	line int
}

// lintBinary returns golangci-lint from PATH or the Go bin directory, or "".
func lintBinary() string {
	if p, err := exec.LookPath("golangci-lint"); err == nil {
		return p
	}
	out, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		return ""
	}
	p, err := exec.LookPath(filepath.Join(strings.TrimSpace(string(out)), "bin", "golangci-lint"))
	if err != nil {
		return ""
	}
	return p
}

// TestLintRulesReportTheirExamples runs the real linter, with the repository's
// own configuration, over the fixtures. Every line carrying a want comment must
// be reported with that text, and no other line may be reported. It uses an
// empty cache because golangci-lint does not key its cache on dev/rules.go.
func TestLintRulesReportTheirExamples(t *testing.T) {
	bin := lintBinary()
	if bin == "" || testing.Short() {
		if os.Getenv(ruleguardModeEnv) == "require" {
			t.Fatal("golangci-lint is required and was not run")
		}
		t.Skip("golangci-lint not installed, or -short")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}

	args := []string{
		"run", "--enable-only", "gocritic", "--output.json.path=stdout", "--show-stats=false",
		"--max-issues-per-linter=0", "--max-same-issues=0", "--uniq-by-line=false", "--path-mode=abs",
	}
	for _, dir := range ruleguardFixtures {
		args = append(args, "./"+dir)
	}
	cmd := exec.Command(bin, args...) //nolint:gosec // the linter this repository pins, with fixed arguments
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOLANGCI_LINT_CACHE="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var exit *exec.ExitError
	// Exit status 1 means findings, which the fixtures are made of.
	if err := cmd.Run(); err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		t.Fatalf("golangci-lint: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	var report struct {
		Issues []struct {
			Text string
			Pos  struct {
				Filename string
				Line     int
			}
		}
	}
	if err := json.NewDecoder(&stdout).Decode(&report); err != nil {
		t.Fatalf("read the report: %v\n%s", err, stderr.String())
	}

	want := wantedFindings(t, root)
	if len(want) == 0 {
		t.Fatal("no want comments found in the fixtures")
	}
	for _, is := range report.Issues {
		rel, err := filepath.Rel(root, is.Pos.Filename)
		if err != nil {
			t.Fatal(err)
		}
		at := lintLine{filepath.ToSlash(rel), is.Pos.Line}
		text, ok := want[at]
		if !ok || !strings.Contains(is.Text, text) {
			t.Errorf("%s:%d: unexpected finding: %s", at.file, at.line, is.Text)
			continue
		}
		delete(want, at)
	}
	for at, text := range want {
		t.Errorf("%s:%d: no finding containing %q", at.file, at.line, text)
	}
}

// wantedFindings reads the want comments of every fixture file.
func wantedFindings(t *testing.T, root string) map[lintLine]string {
	t.Helper()
	want := map[lintLine]string{}
	for _, dir := range ruleguardFixtures {
		entries, err := fs.ReadDir(os.DirFS(root), dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			file := dir + "/" + e.Name()
			data, err := os.ReadFile(filepath.Join(root, file)) //nolint:gosec // a fixture listed above
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				if m := wantComment.FindStringSubmatch(line); m != nil {
					want[lintLine{file, i + 1}] = m[1]
				}
			}
		}
	}
	return want
}

// TestWantCommentsNameTheirLine keeps the fixture reader honest about which line
// a want comment belongs to.
func TestWantCommentsNameTheirLine(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for at := range wantedFindings(t, root) {
		if !strings.HasSuffix(at.file, "/state/bad.go") {
			t.Errorf("want comment outside the bad examples: %s:%s", at.file, strconv.Itoa(at.line))
		}
	}
}
