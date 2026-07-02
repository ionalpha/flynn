package render_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ionalpha/flynn/internal/tui/render"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

func TestHighlightGoKeyword(t *testing.T) {
	rows := render.Highlight(theme.Default(), "go", "func main() {}\n", 80)
	if len(rows) != 1 {
		t.Fatalf("rows %q", rows)
	}
	// Default themes the keyword role magenta (SGR 35).
	if !strings.Contains(rows[0], "35m") {
		t.Fatalf("keyword style missing: %q", rows[0])
	}
	if ansi.Strip(rows[0]) != "func main() {}" {
		t.Fatalf("text mangled: %q", ansi.Strip(rows[0]))
	}
}

func TestHighlightUnknownLanguageFallsBack(t *testing.T) {
	rows := render.Highlight(theme.Default(), "no-such-lang", "plain text\n", 80)
	if len(rows) != 1 || ansi.Strip(rows[0]) != "plain text" {
		t.Fatalf("fallback got %q", rows)
	}
}

func TestHighlightPreservesLineCount(t *testing.T) {
	code := "package x\n\nfunc a() {}\nfunc b() {}\n"
	rows := render.Highlight(theme.Default(), "go", code, 200)
	if len(rows) != 4 {
		t.Fatalf("line count %d, rows %q", len(rows), plain(rows))
	}
}

func TestHighlightHardWraps(t *testing.T) {
	code := strings.Repeat("a", 50) + "\n"
	rows := render.Highlight(theme.Default(), "", code, 20)
	if len(rows) != 3 {
		t.Fatalf("hard wrap got %d rows", len(rows))
	}
	for _, r := range rows {
		if ansi.StringWidth(r) > 20 {
			t.Fatalf("row overflows: %q", r)
		}
	}
}
