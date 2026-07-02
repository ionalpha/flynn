package render_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ionalpha/flynn/internal/tui/render"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// FuzzMarkdown feeds arbitrary bytes and widths through the full markdown
// pipeline and holds the two hard contracts: no panic, and no row wider than
// the terminal.
func FuzzMarkdown(f *testing.F) {
	f.Add("# h\n\npara *em* **st**\n\n- a\n- b\n", 80)
	f.Add("```go\nfunc main() {}\n```\n", 24)
	f.Add("| a | b |\n|---|--:|\n| 1 | 2 |\n", 15)
	f.Add("> q\n\n---\n\n[l](https://e.co)\n", 3)
	f.Add("~~s~~ `c` \\\nhard\n", 1)
	f.Add("宽宽宽", 2)
	// Regression: invalid UTF-8 mixed with emphasis once re-encoded to the
	// replacement character after wrap points were chosen, growing the row
	// past the width.
	f.Add("000000000000000000000000000\U000b467c0000000\x1500000000000000000000000000\x01000 _0000000000\x1a\xb2 00\xbf0\xb2\xd50\x9f0_", 80)
	md := render.NewMarkdown(theme.Default())
	f.Fuzz(func(t *testing.T, source string, width int) {
		width = 1 + abs(width)%160
		for i, r := range md.Render(source, width) {
			if w := ansi.StringWidth(r); w > width {
				t.Fatalf("row %d is %d cells at width %d: %q", i, w, width, r)
			}
			if strings.ContainsRune(r, '\n') {
				t.Fatalf("row %d contains a newline: %q", i, r)
			}
		}
	})
}

// FuzzDiff does the same for both diff layouts.
func FuzzDiff(f *testing.F) {
	f.Add("a\nb\nc\n", "a\nx\nc\n", 80)
	f.Add("", "new file\n", 120)
	f.Add("one\ttab\n", "one tab\n", 40)
	f.Fuzz(func(t *testing.T, before, after string, width int) {
		width = 1 + abs(width)%240
		for i, r := range render.Diff(theme.Default(), "f", before, after, width) {
			if w := ansi.StringWidth(r); w > width {
				t.Fatalf("row %d is %d cells at width %d: %q", i, w, width, r)
			}
		}
	})
}

// FuzzHighlight holds the contracts for direct code highlighting, where the
// language tag itself is untrusted input.
func FuzzHighlight(f *testing.F) {
	f.Add("go", "func main() {}\n", 80)
	f.Add("zzz", "plain\n", 10)
	f.Add("", "\tindent\n", 6)
	f.Fuzz(func(t *testing.T, lang, code string, width int) {
		width = 1 + abs(width)%160
		for i, r := range render.Highlight(theme.Default(), lang, code, width) {
			if w := ansi.StringWidth(r); w > width {
				t.Fatalf("row %d is %d cells at width %d: %q", i, w, width, r)
			}
		}
	})
}

func abs(n int) int {
	if n < 0 {
		if n == -n { // math.MinInt has no positive twin
			return 0
		}
		return -n
	}
	return n
}
