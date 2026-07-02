package render_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ionalpha/flynn/internal/tui/render"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

const (
	diffBefore = "alpha\nbeta\ngamma\ndelta\n"
	diffAfter  = "alpha\nbeta changed\ngamma\ndelta\n"
)

func TestDiffNoChange(t *testing.T) {
	if rows := render.Diff(theme.Default(), "a.go", "same\n", "same\n", 80); rows != nil {
		t.Fatalf("no-op diff rendered %q", rows)
	}
}

func TestDiffUnified(t *testing.T) {
	rows := plain(render.Diff(theme.Default(), "a.go", diffBefore, diffAfter, 80))
	joined := strings.Join(rows, "\n")
	if !strings.Contains(rows[0], "a.go") || !strings.Contains(rows[0], "+1") || !strings.Contains(rows[0], "-1") {
		t.Fatalf("header got %q", rows[0])
	}
	for _, want := range []string{"- beta", "+ beta changed", "@@"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("unified diff missing %q:\n%s", want, joined)
		}
	}
	// Both gutters present on a context line: old and new numbers match.
	if !strings.Contains(joined, "1 1") {
		t.Fatalf("gutter missing:\n%s", joined)
	}
}

func TestDiffSplitOnWideTerminal(t *testing.T) {
	rows := plain(render.Diff(theme.Default(), "a.go", diffBefore, diffAfter, 120))
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "│") {
		t.Fatalf("split view missing column separator:\n%s", joined)
	}
	// The paired change sits on one visual row: old text and new text
	// side by side.
	var found bool
	for _, r := range rows {
		if strings.Contains(r, "- beta") && strings.Contains(r, "+ beta changed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("paired rows not aligned:\n%s", joined)
	}
}

func TestDiffIntralineHighlight(t *testing.T) {
	rows := render.Diff(theme.Default(), "a.go", "keep prefix old suffix\n", "keep prefix new suffix\n", 80)
	var found bool
	for _, r := range rows {
		if strings.Contains(r, "\x1b[7;") || strings.Contains(r, ";7m") || strings.Contains(r, "\x1b[7m") {
			found = true
		}
	}
	if !found {
		t.Fatalf("word-level highlight missing:\n%q", rows)
	}
}

func TestDiffUnpairedRuns(t *testing.T) {
	rows := plain(render.Diff(theme.Default(), "a.go", "one\n", "one\ntwo\nthree\n", 120))
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"+ two", "+ three"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("insert-only run missing %q:\n%s", want, joined)
		}
	}
}

func TestDiffLongLinesWrapUnified(t *testing.T) {
	long := strings.Repeat("x", 200)
	rows := render.Diff(theme.Default(), "a.go", "short\n", long+"\n", 60)
	for _, r := range rows {
		if ansi.StringWidth(r) > 60 {
			t.Fatalf("unified row overflows: %d cells", ansi.StringWidth(r))
		}
	}
	total := 0
	for _, r := range rows {
		total += strings.Count(ansi.Strip(r), "x")
	}
	if total != 200 {
		t.Fatalf("wrapped content lost: %d of 200 x's", total)
	}
}

func TestDiffLongLinesTruncateSplit(t *testing.T) {
	long := strings.Repeat("y", 200)
	rows := render.Diff(theme.Default(), "a.go", "short\n", long+"\n", 120)
	for _, r := range rows {
		if ansi.StringWidth(r) > 120 {
			t.Fatalf("split row overflows: %d cells", ansi.StringWidth(r))
		}
	}
	if !strings.Contains(strings.Join(plain(rows), "\n"), "..") {
		t.Fatalf("split truncation marker missing")
	}
}

func TestDiffTabsExpanded(t *testing.T) {
	rows := plain(render.Diff(theme.Default(), "a.go", "a\n", "a\n\tindented\n", 80))
	if !strings.Contains(strings.Join(rows, "\n"), "    indented") {
		t.Fatalf("tab not expanded:\n%s", strings.Join(rows, "\n"))
	}
}
