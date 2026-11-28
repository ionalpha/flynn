package screen

import (
	"bytes"
	"io"

	"github.com/charmbracelet/x/ansi"
)

// Painter owns the live region: the last few terminal rows, repainted in place. It
// tracks only the lines it last painted and the terminal size, and never queries
// the terminal. After every operation the cursor rests at column one of the last
// live row (or of the row where an empty region will begin).
//
// Movement is relative to that row, which stays correct when the terminal scrolls
// because the cursor and live rows scroll together. Each operation is one write
// wrapped in synchronized output, so the terminal applies it as one frame. Write
// errors are sticky: after the first, every operation is a no-op and Err reports
// it, so a torn-down terminal cannot cascade failures mid-exit.
type Painter struct {
	w      io.Writer
	width  int
	height int
	live   []string
	err    error
}

// NewPainter builds a painter over w, assuming the cursor starts at column one of
// an unused row (a fresh prompt line).
func NewPainter(w io.Writer, width, height int) *Painter {
	p := &Painter{w: w}
	p.Resize(width, height)
	return p
}

// Err returns the first write error the painter hit, or nil.
func (p *Painter) Err() error { return p.err }

// Live returns the painted lines, for tests and diagnostics. Callers must not
// mutate the slice.
func (p *Painter) Live() []string { return p.live }

// Resize records a new terminal size without repainting. The terminal may have
// rewrapped the painted rows, so the caller re-renders at the new width and calls
// Repaint.
func (p *Painter) Resize(width, height int) {
	if width < 1 {
		width = 1
	}
	// At full height, committing finalized lines would push live rows into
	// scrollback mid-frame, so keep one history row above the region.
	if height < 2 {
		height = 2
	}
	p.width, p.height = width, height
}

// Paint rewrites only the rows of frame that differ from the last paint.
func (p *Painter) Paint(frame []string) {
	frame = p.guard(frame)
	if p.err != nil || equalLines(p.live, frame) {
		return
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	p.diff(&b, frame)
	b.WriteString(syncEnd)
	p.flush(&b)
	p.live = frame
}

// Insert commits finalized lines to the terminal's scrollback above the live
// region, then paints frame. Finalized lines become ordinary terminal output, so
// native scrolling, selection and search work on them. Long ones are left for the
// terminal to wrap; only live rows are truncated, because the row arithmetic
// needs one line per row.
func (p *Painter) Insert(finalized, frame []string) {
	frame = p.guard(frame)
	if p.err != nil {
		return
	}
	if len(finalized) == 0 {
		p.Paint(frame)
		return
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	p.clearLive(&b)
	for _, ln := range finalized {
		b.WriteString(ln)
		b.WriteString("\r\n")
	}
	p.writeRows(&b, frame)
	b.WriteString(syncEnd)
	p.flush(&b)
	p.live = frame
}

// Reflow clears the screen and the terminal's scrollback, then writes finalized
// and frame from the top. Rows that have scrolled away cannot be addressed, so
// erasing them is the only way to replace them; whatever the terminal showed
// before the session goes with them.
//
// The screen is erased before the scrollback (ED 2, then ED 3) because Windows
// Terminal, VTE and tmux move the screen into scrollback on ED 2, which in the
// other order would leave a stale copy there.
func (p *Painter) Reflow(finalized, frame []string) {
	frame = p.guard(frame)
	if p.err != nil {
		return
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	b.WriteString(ansi.CursorHomePosition)
	b.WriteString(ansi.EraseDisplay(2))
	b.WriteString(ansi.EraseDisplay(3))
	for _, ln := range finalized {
		b.WriteString(ln)
		b.WriteString("\r\n")
	}
	p.writeRows(&b, frame)
	b.WriteString(syncEnd)
	p.flush(&b)
	p.live = frame
}

// Repaint clears the live region and redraws frame without diffing. It recovers
// from anything that invalidates the diff base: a resize, or output from outside
// the painter such as a child process.
func (p *Painter) Repaint(frame []string) {
	frame = p.guard(frame)
	if p.err != nil {
		return
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	p.clearLive(&b)
	p.writeRows(&b, frame)
	b.WriteString(syncEnd)
	p.flush(&b)
	p.live = frame
}

// Close leaves the live region as scrollback and moves the cursor to a fresh
// line below it. The painter is unusable afterwards except for Err.
func (p *Painter) Close() {
	if p.err != nil {
		return
	}
	var b bytes.Buffer
	if len(p.live) > 0 {
		b.WriteString("\r\n")
	}
	p.flush(&b)
	p.live = nil
}

// guard enforces what the row arithmetic needs: every live line at most one row
// wide and a history row above the region. A component bug can misrender a line
// but not corrupt the geometry of later paints.
func (p *Painter) guard(frame []string) []string {
	maxRows := p.height - 1
	if len(frame) > maxRows {
		frame = frame[len(frame)-maxRows:]
	}
	out := make([]string, len(frame))
	for i, ln := range frame {
		out[i] = Truncate(ln, p.width)
	}
	return out
}

func (p *Painter) diff(b *bytes.Buffer, frame []string) {
	n, m := len(p.live), len(frame)
	if n == 0 {
		p.writeRows(b, frame)
		return
	}
	cur := n - 1 // the resting row
	common := min(n, m)
	for i := range common {
		if p.live[i] == frame[i] {
			continue
		}
		moveTo(b, &cur, i)
		p.writeRow(b, frame[i])
	}
	switch {
	case m > n:
		moveTo(b, &cur, n-1)
		for i := n; i < m; i++ {
			b.WriteString("\r\n")
			cur = i
			p.writeRow(b, frame[i])
		}
	case m < n:
		// Erase the stale rows below the new last row, then rest on it. When
		// the whole region empties, rest where the region will restart.
		moveTo(b, &cur, m)
		b.WriteString("\r")
		b.WriteString(ansi.EraseDisplay(0))
		if m > 0 {
			moveTo(b, &cur, m-1)
		}
	default:
		moveTo(b, &cur, m-1)
		b.WriteString("\r")
	}
}

// clearLive erases from the top of the live region to the end of the screen,
// leaving the cursor where new content begins.
func (p *Painter) clearLive(b *bytes.Buffer) {
	if n := len(p.live); n > 1 {
		b.WriteString(ansi.CursorUp(n - 1))
	}
	b.WriteString("\r")
	b.WriteString(ansi.EraseDisplay(0))
}

func (p *Painter) flush(b *bytes.Buffer) {
	if b.Len() == 0 {
		return
	}
	if _, err := p.w.Write(b.Bytes()); err != nil {
		p.err = err
	}
}

// moveTo moves the cursor from row *cur to target and records the position.
func moveTo(b *bytes.Buffer, cur *int, target int) {
	switch {
	case target < *cur:
		b.WriteString(ansi.CursorUp(*cur - target))
	case target > *cur:
		b.WriteString(ansi.CursorDown(target - *cur))
	}
	*cur = target
}

// writeRow rewrites one row, erasing to its end so no old content survives.
func (p *Painter) writeRow(b *bytes.Buffer, ln string) {
	b.WriteString("\r")
	b.WriteString(ln)
	eraseRest(b, ln, p.width)
}

// writeRows writes frame from the cursor's row, leaving the cursor on the last.
func (p *Painter) writeRows(b *bytes.Buffer, frame []string) {
	for i, ln := range frame {
		if i > 0 {
			b.WriteString("\r\n")
		}
		p.writeRow(b, ln)
	}
}

// eraseRest clears what follows ln on its row. A row that fills the width leaves
// the cursor on its last cell with the wrap pending, where an erase would take the
// final character with it, and has nothing left to clear.
func eraseRest(b *bytes.Buffer, ln string, width int) {
	if Width(ln) < width {
		b.WriteString(ansi.EraseLineRight)
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Synchronized output brackets (DEC private mode 2026); terminals without the
// mode ignore them.
const (
	syncBegin = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
)
