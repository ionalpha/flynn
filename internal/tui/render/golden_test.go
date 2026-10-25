package render_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/render"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// updateGoldenEnv names the environment variable that rewrites the golden
// files from the current renderer instead of comparing against them.
const updateGoldenEnv = "FLYNN_UPDATE_GOLDEN"

// checkGolden compares rows, escape sequences included, with
// testdata/<name>.golden, so a layout change that keeps the plain text intact
// but moves a style or a padding cell still fails.
func checkGolden(t *testing.T, name string, rows []string) {
	t.Helper()
	got := strings.Join(rows, "\n") + "\n"
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv(updateGoldenEnv) != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with %s=1): %v", updateGoldenEnv, err)
	}
	if got != string(want) {
		t.Fatalf("%s differs from the golden file\n got: %q\nwant: %q", name, got, want)
	}
}

func TestDiffGolden(t *testing.T) {
	long := strings.Repeat("z", 150)
	var many strings.Builder
	for i := 1; i <= 120; i++ {
		many.WriteString("line\n")
	}
	cases := []struct {
		name          string
		before, after string
		width         int
	}{
		{"split-replace-paired", "keep\nold one\nold two\ntail\n", "keep\nnew one\nnew two\ntail\n", 120},
		{"split-unequal-runs", "a\nb\nc\nd\n", "a\nX\nd\n", 120},
		{"split-more-inserts", "a\nb\nc\n", "a\nb\nP\nQ\nR\nc\n", 120},
		{"split-delete-only", "a\nb\nc\nd\n", "a\nd\n", 120},
		{"split-insert-only", "a\n", "a\nb\nc\n", 120},
		{"split-multi-hunk", many.String(), strings.Replace(strings.Replace(many.String(), "line\n", "first\n", 1), "line\nline\nline\n", "line\nline\nlast\n", 1) + "\n", 110},
		{"split-long-and-tabs", "short\n\told\n", long + "\n\tnew\r\n", 100},
		{"split-widest-gutter", many.String(), many.String() + "extra\n", 100},
		{"unified-replace", "keep\nold one\nold two\ntail\n", "keep\nnew one\nnew two\ntail\n", 80},
		{"unified-too-narrow", "a\nb\n", "a\nc\n", 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkGolden(t, "diff-"+c.name, render.Diff(theme.Default(), "f.go", c.before, c.after, c.width))
		})
	}
}

func TestMarkdownTableGolden(t *testing.T) {
	cases := []struct {
		name, src string
		width     int
	}{
		{"aligned", "| Name | N | Mid |\n|:-----|--:|:-:|\n| a | 1 | x |\n| bb | 22 | yy |", 40},
		{"ragged-rows", "| A | B |\n|---|---|\n| 1 |\n| 1 | 2 | 3 |", 40},
		{"header-only", "| A | B |\n|---|---|", 40},
		{"inline-styles", "| `code` | **bold** |\n|---|---|\n| *em* | [l](http://x) |", 60},
		{"empty-cells", "| A | B |\n|---|---|\n|   |   |", 20},
		{"shrinks-widest-first", "| aaaaaaaaaaaaaaaaaaaa | bbbbbbbbbb | c |\n|---|---|---|\n| x | y | z |", 30},
		{"below-floor", "| aaaaaaaaaa | bbbbbbbbbb | cccccccccc |\n|---|---|---|\n| x | y | z |", 8},
		{"wide-runes", "| 名前 | n |\n|---|---|\n| 日本語 | 1 |", 40},
		{"centered-truncated", "| h |\n|:-:|\n| abcdefghijklmnop |", 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkGolden(t, "table-"+c.name, render.NewMarkdown(theme.Default()).Render(c.src, c.width))
		})
	}
}
