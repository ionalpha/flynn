package screen_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/screen"
)

// TestWrapKeepsWordsWhole wraps generated words, many of them hyphenated, at a
// run of widths: no row is wider than the width, a word that fits stays on one
// row, and no character is lost.
func TestWrapKeepsWordsWhole(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		words := rapid.SliceOfN(rapid.StringMatching(`[a-z世]{1,9}([-/.][a-z]{1,9}){0,3}`), 1, 30).Draw(rt, "words")
		src := strings.Join(words, " ")
		for _, width := range rapid.SliceOfN(rapid.IntRange(2, 60), 1, 8).Draw(rt, "widths") {
			rows := strings.Split(screen.Wrap(src, width), "\n")
			var got []string
			for _, row := range rows {
				if screen.Width(row) > width {
					rt.Fatalf("row %q is wider than %d", row, width)
				}
				got = append(got, strings.Fields(row)...)
			}
			for _, w := range words {
				if screen.Width(w) <= width && !slices.Contains(got, w) {
					rt.Fatalf("%q fits %d cells but was split: %q", w, width, rows)
				}
			}
			if strings.Join(got, "") != strings.Join(words, "") {
				rt.Fatalf("text changed at %d: %q", width, rows)
			}
		}
	})
}

func TestWrap(t *testing.T) {
	for _, c := range []struct {
		in    string
		width int
		want  string
	}{
		{"a semi-trusted host", 12, "a\nsemi-trusted\nhost"},
		{"  indented  text", 20, "  indented  text"},
		{"    deep", 6, "deep"},
		{"one\n\ntwo ", 10, "one\n\ntwo"},
		{"abcdefgh", 3, "abc\ndef\ngh"},
		{"left alone", 0, "left alone"},
	} {
		if got := screen.Wrap(c.in, c.width); got != c.want {
			t.Errorf("Wrap(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

// A row that fills the width is not followed by an erase, which would take its
// last character while the terminal's wrap is pending.
func TestFullWidthRowsAreNotErased(t *testing.T) {
	var inline, alt bytes.Buffer
	screen.NewPainter(&inline, 5, 4).Paint([]string{"abcde", "ab"})
	screen.NewAltPainter(&alt, 5, 4).Paint([]string{"abcde"})
	for _, out := range []string{inline.String(), alt.String()} {
		if strings.Contains(out, "abcde\x1b[K") {
			t.Errorf("erase after a full row: %q", out)
		}
	}
	if !strings.Contains(inline.String(), "ab\x1b[K") {
		t.Errorf("short row not erased: %q", inline.String())
	}
}

// Write errors are sticky, so a reflow on a torn-down terminal does nothing.
func TestReflowAfterAWriteError(t *testing.T) {
	p := screen.NewPainter(failWriter{}, 20, 6)
	p.Paint([]string{"live"})
	p.Reflow([]string{"new"}, []string{"other"})
	if live := p.Live(); len(live) != 0 && live[0] == "other" {
		t.Errorf("reflow ran after a write error: %q", live)
	}
}

func TestReflowReplacesEverythingCommitted(t *testing.T) {
	var out bytes.Buffer
	p := screen.NewPainter(&out, 20, 6)
	p.Insert([]string{"old"}, []string{"live"})
	out.Reset()
	p.Reflow([]string{"new one", "new two"}, []string{"live"})
	want := "\x1b[?2026h\x1b[H\x1b[2J\x1b[3Jnew one\r\nnew two\r\n\rlive\x1b[K\x1b[?2026l"
	if out.String() != want {
		t.Errorf("inline reflow wrote %q, want %q", out.String(), want)
	}

	out.Reset()
	a := screen.NewAltPainter(&out, 20, 6)
	a.Insert([]string{"old"}, []string{"live"})
	out.Reset()
	a.Reflow([]string{"new"}, []string{"live"})
	if got := out.String(); strings.Contains(got, "old") || !strings.Contains(got, "new") {
		t.Errorf("alternate screen reflow wrote %q", got)
	}
}
