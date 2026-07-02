package screen_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/screen"
)

// esc rewrites readable markers into the escape bytes the painter emits, so
// the expected output in each test stays legible.
func esc(s string) string {
	r := strings.NewReplacer(
		"<sync>", "\x1b[?2026h",
		"</sync>", "\x1b[?2026l",
		"<el>", "\x1b[K",
		"<ed>", "\x1b[J",
		"<up>", "\x1b[A",
		"<up2>", "\x1b[2A",
		"<down>", "\x1b[B",
		"<cr>", "\r",
		"<nl>", "\r\n",
	)
	return r.Replace(s)
}

func TestPaintFirstFrameWritesEveryRow(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one", "two"})
	want := esc("<sync><cr>one<el><nl><cr>two<el></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("first paint:\n got %q\nwant %q", got, want)
	}
}

func TestPaintIdenticalFrameWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one", "two"})
	buf.Reset()
	p.Paint([]string{"one", "two"})
	if buf.Len() != 0 {
		t.Fatalf("repainting an identical frame wrote %q", buf.String())
	}
}

func TestPaintRewritesOnlyChangedRows(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one", "two", "three"})
	buf.Reset()
	p.Paint([]string{"one", "TWO", "three"})
	// From the resting row (index 2) up one to row 1, rewrite it, then rest
	// back on the last row.
	want := esc("<sync><up><cr>TWO<el><down><cr></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("single-row change:\n got %q\nwant %q", got, want)
	}
}

func TestPaintGrowsTheRegion(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one"})
	buf.Reset()
	p.Paint([]string{"one", "two", "three"})
	want := esc("<sync><nl><cr>two<el><nl><cr>three<el></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("grow:\n got %q\nwant %q", got, want)
	}
}

func TestPaintShrinksTheRegion(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one", "two", "three"})
	buf.Reset()
	p.Paint([]string{"one"})
	// Move from row 2 to row 1 (the first stale row), erase to the end of the
	// screen, then rest on the last remaining row.
	want := esc("<sync><up><cr><ed><up></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("shrink:\n got %q\nwant %q", got, want)
	}
}

func TestPaintEmptyFrameClearsTheRegion(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one", "two"})
	buf.Reset()
	p.Paint(nil)
	want := esc("<sync><up><cr><ed></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("clear:\n got %q\nwant %q", got, want)
	}
	buf.Reset()
	// The region restarts from the resting row as if freshly created.
	p.Paint([]string{"back"})
	want = esc("<sync><cr>back<el></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("restart after clear:\n got %q\nwant %q", got, want)
	}
}

func TestInsertCommitsAboveTheLiveRegion(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"status", "composer"})
	buf.Reset()
	p.Insert([]string{"final A", "final B"}, []string{"status", "composer"})
	// Up to the region top, clear to screen end, print the finalized lines as
	// ordinary output, then redraw the live region below them.
	want := esc("<sync><up><cr><ed>final A<nl>final B<nl><cr>status<el><nl><cr>composer<el></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("insert:\n got %q\nwant %q", got, want)
	}
}

func TestInsertWithNothingFinalizedIsAPaint(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one"})
	buf.Reset()
	p.Insert(nil, []string{"one"})
	if buf.Len() != 0 {
		t.Fatalf("no-op insert wrote %q", buf.String())
	}
}

func TestGuardTruncatesWideRowsAndCapsHeight(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 5, 3) // live region capped at 2 rows
	p.Paint([]string{"aaaaaaaaaa", "b", "c"})
	live := p.Live()
	if len(live) != 2 {
		t.Fatalf("live region rows = %d, want the height cap 2", len(live))
	}
	// The cap keeps the tail of the frame (the composer end), and every row
	// fits the width.
	if live[0] != "b" || live[1] != "c" {
		t.Fatalf("live = %q, want the frame tail [b c]", live)
	}
	buf.Reset()
	p.Paint([]string{"wide行here", "x"})
	if got := p.Live()[0]; screen.Width(got) > 5 {
		t.Fatalf("guard let a %d-cell row through at width 5: %q", screen.Width(got), got)
	}
}

func TestResizeThenRepaintRedrawsWithoutDiffing(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one", "two"})
	p.Resize(10, 10)
	buf.Reset()
	p.Repaint([]string{"one", "two"})
	want := esc("<sync><up><cr><ed><cr>one<el><nl><cr>two<el></sync>")
	if got := buf.String(); got != want {
		t.Fatalf("repaint:\n got %q\nwant %q", got, want)
	}
}

func TestWriteErrorIsStickyAndReported(t *testing.T) {
	p := screen.NewPainter(failWriter{}, 20, 10)
	p.Paint([]string{"one"})
	if p.Err() == nil {
		t.Fatal("write error was not reported")
	}
	// Later operations are no-ops, not panics or fresh errors.
	p.Paint([]string{"two"})
	p.Insert([]string{"x"}, []string{"y"})
	p.Close()
	if !errors.Is(p.Err(), errBoom) {
		t.Fatalf("Err = %v, want the original write error", p.Err())
	}
}

var errBoom = errors.New("boom")

type failWriter struct{}

func (failWriter) Write(_ []byte) (int, error) { return 0, errBoom }

func TestCloseLeavesTheCursorBelowTheRegion(t *testing.T) {
	var buf bytes.Buffer
	p := screen.NewPainter(&buf, 20, 10)
	p.Paint([]string{"one"})
	buf.Reset()
	p.Close()
	if got, want := buf.String(), esc("<nl>"); got != want {
		t.Fatalf("close:\n got %q\nwant %q", got, want)
	}
}
