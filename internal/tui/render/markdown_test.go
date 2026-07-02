package render_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ionalpha/flynn/internal/tui/render"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

func plain(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = ansi.Strip(r)
	}
	return out
}

func renderPlain(t *testing.T, source string, width int) []string {
	t.Helper()
	return plain(render.NewMarkdown(theme.Default()).Render(source, width))
}

func TestMarkdownHeading(t *testing.T) {
	rows := renderPlain(t, "## Title", 40)
	if len(rows) != 1 || rows[0] != "## Title" {
		t.Fatalf("heading rendered %q", rows)
	}
}

func TestMarkdownParagraphWraps(t *testing.T) {
	rows := renderPlain(t, "alpha beta gamma delta", 11)
	want := []string{"alpha beta", "gamma delta"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("wrap got %q want %q", rows, want)
	}
}

func TestMarkdownBlankLineBetweenBlocks(t *testing.T) {
	rows := renderPlain(t, "one\n\ntwo", 40)
	want := []string{"one", "", "two"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("blocks got %q", rows)
	}
}

func TestMarkdownLists(t *testing.T) {
	rows := renderPlain(t, "- alpha\n- beta\n  - nested\n\n1. one\n2. two", 40)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"• alpha", "• beta", "  • nested", "1. one", "2. two"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("lists missing %q in:\n%s", want, joined)
		}
	}
}

func TestMarkdownOrderedListStart(t *testing.T) {
	rows := renderPlain(t, "3. three\n4. four", 40)
	if !strings.Contains(strings.Join(rows, "\n"), "3. three") {
		t.Fatalf("ordered start lost: %q", rows)
	}
}

func TestMarkdownListContinuationIndent(t *testing.T) {
	rows := renderPlain(t, "- alpha beta gamma", 10)
	if len(rows) < 2 {
		t.Fatalf("expected wrapped item, got %q", rows)
	}
	if rows[0] != "• alpha" || rows[1] != "  beta" {
		t.Fatalf("continuation misaligned: %q", rows)
	}
}

func TestMarkdownTaskList(t *testing.T) {
	rows := renderPlain(t, "- [x] done\n- [ ] open", 40)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "[x] done") || !strings.Contains(joined, "[ ] open") {
		t.Fatalf("task list got:\n%s", joined)
	}
}

func TestMarkdownBlockquote(t *testing.T) {
	rows := renderPlain(t, "> quoted text", 40)
	if len(rows) != 1 || rows[0] != "│ quoted text" {
		t.Fatalf("blockquote got %q", rows)
	}
}

func TestMarkdownCodeBlockIndented(t *testing.T) {
	rows := renderPlain(t, "```go\nfunc main() {}\n```", 40)
	if len(rows) != 1 || rows[0] != "  func main() {}" {
		t.Fatalf("code block got %q", rows)
	}
}

func TestMarkdownCodeBlockKeepsBlankLines(t *testing.T) {
	rows := renderPlain(t, "```\na\n\nb\n```", 40)
	want := []string{"  a", "  ", "  b"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("code blanks got %q want %q", rows, want)
	}
}

func TestMarkdownThematicBreak(t *testing.T) {
	rows := renderPlain(t, "---", 10)
	if len(rows) != 1 || rows[0] != strings.Repeat("─", 10) {
		t.Fatalf("rule got %q", rows)
	}
}

func TestMarkdownLinkShowsDestination(t *testing.T) {
	rows := renderPlain(t, "[docs](https://example.com)", 60)
	if len(rows) != 1 || rows[0] != "docs (https://example.com)" {
		t.Fatalf("link got %q", rows)
	}
}

func TestMarkdownAutolink(t *testing.T) {
	rows := renderPlain(t, "see https://example.com now", 60)
	if len(rows) != 1 || rows[0] != "see https://example.com now" {
		t.Fatalf("autolink got %q", rows)
	}
}

func TestMarkdownTable(t *testing.T) {
	src := "| Name | N |\n|:-----|--:|\n| a | 1 |\n| bb | 22 |"
	rows := renderPlain(t, src, 40)
	if len(rows) != 4 {
		t.Fatalf("table rows: %q", rows)
	}
	if !strings.Contains(rows[0], "Name") || !strings.Contains(rows[0], "│") {
		t.Fatalf("table header got %q", rows[0])
	}
	if !strings.Contains(rows[1], "┼") {
		t.Fatalf("table separator got %q", rows[1])
	}
	// The N column is right-aligned: 1 sits at the column's right edge.
	if !strings.Contains(rows[2], " 1") {
		t.Fatalf("right alignment lost: %q", rows[2])
	}
}

func TestMarkdownTableNarrowTruncates(t *testing.T) {
	src := "| aaaaaaaaaaaaaaaaaaaa | bbbbbbbbbbbbbbbbbbbb |\n|---|---|\n| x | y |"
	rows := render.NewMarkdown(theme.Default()).Render(src, 20)
	for _, r := range rows {
		if ansi.StringWidth(r) > 20 {
			t.Fatalf("table row wider than terminal: %q", ansi.Strip(r))
		}
	}
}

func TestMarkdownHardBreak(t *testing.T) {
	rows := renderPlain(t, "one\\\ntwo", 40)
	want := []string{"one", "two"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("hard break got %q", rows)
	}
}

func TestMarkdownStrikethroughStyled(t *testing.T) {
	rows := render.NewMarkdown(theme.Default()).Render("~~gone~~", 40)
	if len(rows) != 1 || !strings.Contains(rows[0], "\x1b[9m") {
		t.Fatalf("strikethrough SGR missing: %q", rows)
	}
}

func TestMarkdownNestedStylesCompose(t *testing.T) {
	rows := render.NewMarkdown(theme.Default()).Render("**bold _both_**", 40)
	if len(rows) != 1 {
		t.Fatalf("rows %q", rows)
	}
	// The nested run carries bold and italic in one self-contained span.
	if !strings.Contains(rows[0], "\x1b[1;3m") {
		t.Fatalf("merged style missing: %q", rows[0])
	}
}

func TestMarkdownControlBytesStripped(t *testing.T) {
	rows := render.NewMarkdown(theme.Default()).Render("a\x07b\r", 40)
	for _, r := range rows {
		if strings.ContainsAny(r, "\x07\r") {
			t.Fatalf("control byte survived: %q", r)
		}
	}
}

func TestMarkdownInvalidUTF8Normalized(t *testing.T) {
	// Invalid bytes become the replacement character before any width is
	// measured, so wrap accounting and the emitted row always agree.
	for _, width := range []int{5, 12, 81} {
		rows := render.NewMarkdown(theme.Default()).Render("ab\xbfcd _e\xb2f_ g\xd5\x00h", width)
		for i, r := range rows {
			if w := ansi.StringWidth(r); w > width {
				t.Fatalf("row %d is %d cells at width %d: %q", i, w, width, r)
			}
		}
	}
}

func TestMarkdownZeroWidth(t *testing.T) {
	if rows := render.NewMarkdown(theme.Default()).Render("hello", 0); rows != nil {
		t.Fatalf("width 0 rendered %q", rows)
	}
}

func TestMarkdownOverflowGuard(t *testing.T) {
	// A grapheme wider than the terminal cannot be split; the guard clamps.
	rows := render.NewMarkdown(theme.Default()).Render("宽", 1)
	for _, r := range rows {
		if ansi.StringWidth(r) > 1 {
			t.Fatalf("overflow guard failed: %q", r)
		}
	}
}
