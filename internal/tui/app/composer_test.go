package app

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/editor"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

const cursorOn = "\x1b[7m"

func TestComposerGuttersFirstAndContinuationRows(t *testing.T) {
	var ed editor.Editor
	ed.Insert("first\nsecond")
	rows := composerRows(&ed, theme.Mono(), 40, "", "")
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if !strings.Contains(rows[0], "> ") || !strings.Contains(rows[0], "first") {
		t.Fatalf("first row %q lacks the prompt gutter", rows[0])
	}
	if strings.Contains(rows[1], ">") {
		t.Fatalf("continuation row %q repeats the prompt marker", rows[1])
	}
}

func TestComposerShowsTheCursorExactlyOnce(t *testing.T) {
	var ed editor.Editor
	ed.Insert("hello")
	ed.Left()
	ed.Left()
	rows := composerRows(&ed, theme.Mono(), 40, "", "")
	joined := strings.Join(rows, "\n")
	if n := strings.Count(joined, cursorOn); n != 1 {
		t.Fatalf("got %d cursor cells, want 1 in %q", n, joined)
	}
	// The cursor sits over the "l" two cells back from the end.
	if !strings.Contains(joined, cursorOn+"l") {
		t.Fatalf("cursor is not over the expected cell: %q", joined)
	}
}

func TestComposerCursorRestsPastTheEnd(t *testing.T) {
	var ed editor.Editor
	ed.Insert("ab")
	rows := composerRows(&ed, theme.Mono(), 40, "", "")
	if !strings.Contains(rows[0], cursorOn+" ") {
		t.Fatalf("end-of-line cursor missing in %q", rows[0])
	}
}

func TestComposerCursorCoversAWholeCluster(t *testing.T) {
	const firefighter = "\U0001F469\u200d\U0001F692" // woman firefighter ZWJ sequence
	var ed editor.Editor
	ed.Insert("a" + firefighter + "b")
	ed.Left()
	ed.Left()
	rows := composerRows(&ed, theme.Mono(), 40, "", "")
	if !strings.Contains(rows[0], cursorOn+firefighter) {
		t.Fatalf("cursor split the grapheme cluster: %q", rows[0])
	}
}

func TestComposerPlaceholderOnEmptyBuffer(t *testing.T) {
	var ed editor.Editor
	rows := composerRows(&ed, theme.Mono(), 40, "ask anything", "")
	if len(rows) != 1 || !strings.Contains(rows[0], "ask anything") {
		t.Fatalf("placeholder missing: %v", rows)
	}
	if !strings.Contains(rows[0], cursorOn) {
		t.Fatalf("placeholder row has no cursor: %q", rows[0])
	}
}

func TestComposerRowsNeverExceedTheWidth(t *testing.T) {
	var ed editor.Editor
	ed.Insert("a long prompt that must wrap across rows")
	for _, width := range []int{3, 7, 12, 40} {
		for _, row := range composerRows(&ed, theme.Mono(), width, "", "") {
			if w := screen.Width(row); w > width {
				t.Fatalf("row %q is %d cells wide at width %d", row, w, width)
			}
		}
	}
}

func TestComposerMarkerReplacesTheFirstRowGutterOnly(t *testing.T) {
	var ed editor.Editor
	ed.Insert("!make test\nsecond")
	rows := composerRows(&ed, theme.Mono(), 40, "", "! ")
	if !strings.Contains(rows[0], "! ") || strings.Contains(rows[0], ">") {
		t.Fatalf("first row %q does not carry the marker", rows[0])
	}
	if strings.Contains(rows[1], "!") {
		t.Fatalf("continuation row %q repeats the marker", rows[1])
	}
}

func TestComposerMarkerIsFittedToTheGutter(t *testing.T) {
	var ed editor.Editor
	ed.Insert("x")
	for _, marker := range []string{"!", "!!!", "! "} {
		rows := composerRows(&ed, theme.Mono(), 40, "", marker)
		plain := stripSGR(rows[0])
		if !strings.HasPrefix(plain, "!") || !strings.Contains(plain, "x") {
			t.Fatalf("marker %q rendered as %q", marker, plain)
		}
		if idx := strings.Index(plain, "x"); idx != gutterWidth {
			t.Fatalf("marker %q shifted the body to column %d in %q", marker, idx, plain)
		}
	}
}

// stripSGR removes styling escape sequences, leaving the printed cells.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestHistoryRoundTrip(t *testing.T) {
	var h history
	h.add("one")
	h.add("two")
	h.add("two") // consecutive duplicate collapses

	if got, ok := h.prev("draft"); !ok || got != "two" {
		t.Fatalf("prev = %q, %v", got, ok)
	}
	if got, ok := h.prev(""); !ok || got != "one" {
		t.Fatalf("prev = %q, %v", got, ok)
	}
	if _, ok := h.prev(""); ok {
		t.Fatal("prev past the oldest entry succeeded")
	}
	if got, ok := h.next(); !ok || got != "two" {
		t.Fatalf("next = %q, %v", got, ok)
	}
	if got, ok := h.next(); !ok || got != "draft" {
		t.Fatalf("next did not restore the draft: %q, %v", got, ok)
	}
	if _, ok := h.next(); ok {
		t.Fatal("next past the draft succeeded")
	}
}
