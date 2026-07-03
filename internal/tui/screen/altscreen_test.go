package screen_test

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/screen"
)

// altEsc rewrites readable markers into the escape bytes the alternate-screen
// painter emits, so the expected output in each test stays legible.
func altEsc(s string) string {
	r := strings.NewReplacer(
		"<sync>", "\x1b[?2026h",
		"</sync>", "\x1b[?2026l",
		"<clr>", "\x1b[2J",
		"<el>", "\x1b[K",
		"<h1>", "\x1b[H",
		"<h2>", "\x1b[2;1H",
		"<h3>", "\x1b[3;1H",
		"<h4>", "\x1b[4;1H",
	)
	return r.Replace(s)
}

func TestAltFirstPaintClearsThenWritesEveryRow(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewAltPainter(&buf, 20, 4)
	p.Paint([]string{"one", "two"})
	// A dirty first frame clears the viewport, then writes each non-blank row at
	// its absolute row. The two trailing blank rows need no write against the
	// freshly cleared screen.
	want := altEsc("<sync><clr><h1>one<el><h2>two<el></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("first paint:\n got %q\nwant %q", got, want)
	}
}

func TestAltIdenticalFrameWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewAltPainter(&buf, 20, 4)
	p.Paint([]string{"one", "two"})
	buf.Reset()
	p.Paint([]string{"one", "two"})
	if buf.Len() != 0 {
		t.Fatalf("repainting an identical frame wrote %q", buf.String())
	}
}

func TestAltPaintRewritesOnlyChangedRows(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewAltPainter(&buf, 20, 4)
	p.Paint([]string{"one", "two"})
	buf.Reset()
	p.Paint([]string{"one", "TWO"})
	// Only row two changed; row one and the blanks are left alone. No clear,
	// because the diff base is still trusted.
	want := altEsc("<sync><h2>TWO<el></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("single-row change:\n got %q\nwant %q", got, want)
	}
}

func TestAltInsertShowsTailWithComposerAtBottom(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewAltPainter(&buf, 20, 3)
	p.Paint([]string{"comp"})
	// Commit more transcript than the viewport can hold; the oldest line scrolls
	// off the top and the composer stays on the bottom row.
	p.Insert([]string{"f1", "f2", "f3"}, []string{"comp"})
	vt := newAltVT(3)
	vt.write(buf.String())
	if got, want := vt.screen(), []string{"f2", "f3", "comp"}; !equal(got, want) {
		t.Fatalf("viewport tail:\n got %q\nwant %q", got, want)
	}
}

func TestAltViewportTruncatesToWidth(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewAltPainter(&buf, 5, 3)
	p.Paint([]string{"abcdefgh"})
	vt := newAltVT(3)
	vt.write(buf.String())
	if got := vt.screen(); len(got) != 1 || got[0] != "abcde" {
		t.Fatalf("row not truncated to width 5: %q", got)
	}
}

func TestAltResizeForcesFullRedraw(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewAltPainter(&buf, 20, 4)
	p.Paint([]string{"one", "two"})
	p.Resize(20, 5)
	buf.Reset()
	// After a resize the diff base is stale, so the next frame clears and
	// redraws even though the live content is unchanged.
	p.Paint([]string{"one", "two"})
	if got := buf.String(); !strings.Contains(got, "\x1b[2J") {
		t.Fatalf("resize did not force a clear:\n got %q", got)
	}
}

func TestAltWriteErrorIsStickyAndReported(t *testing.T) {
	p := screen.NewAltPainter(failWriter{}, 20, 4)
	p.Paint([]string{"one"})
	if p.Err() == nil {
		t.Fatal("write error was not reported")
	}
	// Later operations are no-ops, not panics or fresh errors.
	p.Paint([]string{"two"})
	p.Insert([]string{"x"}, []string{"y"})
	p.Repaint([]string{"z"})
	p.Close()
	if !errors.Is(p.Err(), errBoom) {
		t.Fatalf("Err = %v, want the original write error", p.Err())
	}
}

// altVT is a miniature virtual terminal for the alternate-screen painter's
// vocabulary: it interprets absolute cursor positioning, erase-line-right,
// full-screen erase, and the synchronized-output markers over a fixed grid.
// The painter never emits carriage returns or line feeds (it addresses every
// row absolutely), so those are not modelled.
type altVT struct {
	rows     []string
	row, col int
}

func newAltVT(height int) *altVT { return &altVT{rows: make([]string, height)} }

func (v *altVT) write(s string) {
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i += v.escape(s[i:])
			continue
		}
		v.putByte(s[i])
		i++
	}
}

func (v *altVT) escape(s string) int {
	if len(s) < 3 || s[1] != '[' {
		panic("altVT: non-CSI escape: " + strconv.Quote(s))
	}
	j := 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j == len(s) {
		panic("altVT: unterminated escape: " + strconv.Quote(s))
	}
	params, final := s[2:j], s[j]
	if strings.HasPrefix(params, "?") {
		return j + 1 // private mode (synchronized output): display-neutral
	}
	switch final {
	case 'H': // absolute cursor position: row;col, 1-based, default 1
		row, col := 1, 1
		if a, b, ok := strings.Cut(params, ";"); ok {
			row, col = atoiDefault(a, 1), atoiDefault(b, 1)
		} else if params != "" {
			row = atoiDefault(params, 1)
		}
		v.row, v.col = row-1, col-1
	case 'K': // erase from the cursor to the end of the line
		if v.row >= 0 && v.row < len(v.rows) && v.col <= len(v.rows[v.row]) {
			v.rows[v.row] = v.rows[v.row][:v.col]
		}
	case 'J': // full-screen erase (the painter only ever emits ED 2)
		if params != "2" {
			panic("altVT: unexpected erase-display param " + strconv.Quote(params))
		}
		for r := range v.rows {
			v.rows[r] = ""
		}
	default:
		panic("altVT: unexpected final byte " + string(final))
	}
	return j + 1
}

func (v *altVT) putByte(c byte) {
	if v.row < 0 || v.row >= len(v.rows) {
		panic("altVT: write outside the viewport")
	}
	for len(v.rows[v.row]) < v.col {
		v.rows[v.row] += " "
	}
	if v.col < len(v.rows[v.row]) {
		v.rows[v.row] = v.rows[v.row][:v.col] + string(c) + v.rows[v.row][v.col+1:]
	} else {
		v.rows[v.row] += string(c)
	}
	v.col++
}

func (v *altVT) screen() []string {
	end := len(v.rows)
	for end > 0 && v.rows[end-1] == "" {
		end--
	}
	return v.rows[:end]
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		panic("altVT: bad number " + strconv.Quote(s))
	}
	return n
}

// TestProp_AltConvergesToTheTail is the alternate-screen painter's core
// contract: after any sequence of paints, insertions, and repaints, a terminal
// interpreting its output shows exactly the tail of the transcript followed by
// the live region, top-aligned into the viewport. The row diff is therefore
// unobservable: skipping unchanged rows never leaves a stale cell behind.
func TestProp_AltConvergesToTheTail(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		const width, height = 20, 5
		var out bytes.Buffer
		p := screen.NewAltPainter(&out, width, height)
		vt := newAltVT(height)

		var history, live []string
		steps := rapid.IntRange(1, 12).Draw(rt, "steps")
		for range steps {
			switch rapid.IntRange(0, 3).Draw(rt, "op") {
			case 0, 1: // Paint dominates real usage
				live = genFrame(rt, width, height)
				p.Paint(live)
			case 2:
				finalized := genFrame(rt, width, 3)
				live = genFrame(rt, width, height)
				p.Insert(finalized, live)
				history = append(history, finalized...)
			case 3:
				p.Repaint(live)
			}
			if p.Err() != nil {
				rt.Fatalf("painter error: %v", p.Err())
			}
			vt.write(out.String())
			out.Reset()

			want := altWindow(history, live, width, height)
			if got := vt.screen(); !equal(got, want) {
				rt.Fatalf("viewport diverged\n got %q\nwant %q", got, want)
			}
		}
	})
}

// altWindow is the reference viewport: the tail of history followed by live,
// each truncated to width, top-aligned, trailing blanks trimmed.
func altWindow(history, live []string, width, height int) []string {
	combined := append(append([]string{}, history...), live...)
	start := 0
	if len(combined) > height {
		start = len(combined) - height
	}
	combined = combined[start:]
	if len(combined) > height {
		combined = combined[:height]
	}
	out := make([]string, len(combined))
	for i, ln := range combined {
		out[i] = screen.Truncate(ln, width)
	}
	return trimTrailingBlank(out)
}
