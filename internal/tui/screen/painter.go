package screen

import (
	"bytes"
	"io"

	"github.com/charmbracelet/x/ansi"
)

// Painter owns the live region: the last few rows of the terminal, repainted
// in place as the session streams. It tracks only the lines it last painted
// and the terminal size; it never queries the terminal. The cursor contract
// is simple and holds across every operation: the cursor rests at column one
// of the last live row (or at column one of the row where the live region
// will begin, while the region is empty).
//
// All movement is relative (cursor up and down from that resting row), never
// absolute. Relative movement stays correct when the terminal scrolls,
// because the cursor and the live rows scroll together; absolute addressing
// would not.
//
// Each operation is accumulated into one buffer and flushed as a single
// write wrapped in synchronized output, so the terminal applies it as one
// atomic frame. Write errors are sticky: after the first failure every later
// operation is a no-op and Err reports the cause, so a torn-down terminal
// does not produce a cascade of secondary failures mid-exit.
type Painter struct {
	w      io.Writer
	width  int
	height int
	live   []string
	err    error
}

// NewPainter builds a painter over w for a terminal of the given size. The
// painter assumes the cursor starts at column one of an otherwise unused row
// (a fresh prompt line); the first Paint draws the live region from there.
func NewPainter(w io.Writer, width, height int) *Painter {
	p := &Painter{w: w}
	p.Resize(width, height)
	return p
}

// Err returns the first write error the painter hit, or nil.
func (p *Painter) Err() error { return p.err }

// Live returns the lines currently painted in the live region. It exists for
// tests and diagnostics; callers must not mutate the returned slice.
func (p *Painter) Live() []string { return p.live }

// Resize records a new terminal size. It does not repaint: after a resize the
// previously painted rows may have been rewrapped by the terminal, so the
// caller re-renders its components at the new width and calls Repaint, which
// redraws without trusting the stale diff base.
func (p *Painter) Resize(width, height int) {
	if width < 1 {
		width = 1
	}
	// The live region never owns the whole screen: committing finalized lines
	// scrolls the screen, and at full height that would push live rows into
	// scrollback mid-frame. Keeping height at two or more leaves at least one
	// history row above the region.
	if height < 2 {
		height = 2
	}
	p.width, p.height = width, height
}

// Paint diffs frame against the last painted live region and rewrites only
// the rows that changed. Painting an identical frame writes nothing.
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

// Insert commits finalized lines into the terminal's own scrollback above the
// live region, then paints frame as the new live region. The finalized lines
// become ordinary terminal output: they are never repainted again, and native
// scrolling, selection, and search work on them. Long finalized lines are
// left to the terminal to wrap, matching how any other program's output
// behaves in scrollback; only live rows are hard-truncated, because the
// painter's row arithmetic depends on one live line per row.
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
	writeRows(&b, frame)
	b.WriteString(syncEnd)
	p.flush(&b)
	p.live = frame
}

// Repaint clears the live region and redraws frame from scratch, without
// diffing. It is the recovery path after anything that invalidates the diff
// base: a resize (the terminal rewrapped the old rows) or output from outside
// the painter (a child process wrote to the terminal).
func (p *Painter) Repaint(frame []string) {
	frame = p.guard(frame)
	if p.err != nil {
		return
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	p.clearLive(&b)
	writeRows(&b, frame)
	b.WriteString(syncEnd)
	p.flush(&b)
	p.live = frame
}

// Close finalizes the session's last frame: the live region's current content
// is left in place as ordinary scrollback and the cursor moves to a fresh
// line below it, where the shell prompt will appear. The painter is unusable
// afterwards except for Err.
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

// guard enforces the invariants the row arithmetic depends on: every live
// line is at most one row wide, and the region leaves at least one history
// row above it. A component bug can therefore misrender a line, but it cannot
// corrupt the geometry of everything painted after it.
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

// diff writes the minimal row updates turning the painted region into frame.
func (p *Painter) diff(b *bytes.Buffer, frame []string) {
	n, m := len(p.live), len(frame)
	if n == 0 {
		writeRows(b, frame)
		return
	}
	cur := n - 1 // the resting row
	common := min(n, m)
	for i := range common {
		if p.live[i] == frame[i] {
			continue
		}
		moveTo(b, &cur, i)
		writeRow(b, frame[i])
	}
	switch {
	case m > n:
		moveTo(b, &cur, n-1)
		for i := n; i < m; i++ {
			b.WriteString("\r\n")
			cur = i
			writeRow(b, frame[i])
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

// clearLive moves the cursor to the top of the live region and erases from
// there to the end of the screen, leaving the cursor at column one of the
// row where new content begins.
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

// moveTo emits relative cursor movement from row *cur to row target within
// the live region and records the new position.
func moveTo(b *bytes.Buffer, cur *int, target int) {
	switch {
	case target < *cur:
		b.WriteString(ansi.CursorUp(*cur - target))
	case target > *cur:
		b.WriteString(ansi.CursorDown(target - *cur))
	}
	*cur = target
}

// writeRow rewrites one live row in place: carriage return, the new content,
// then erase to the end of the row so no residue of the old content survives.
func writeRow(b *bytes.Buffer, ln string) {
	b.WriteString("\r")
	b.WriteString(ln)
	b.WriteString(ansi.EraseLineRight)
}

// writeRows writes frame starting at the cursor's row, one row per line,
// leaving the cursor resting on the last row written.
func writeRows(b *bytes.Buffer, frame []string) {
	for i, ln := range frame {
		if i > 0 {
			b.WriteString("\r\n")
		}
		writeRow(b, ln)
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

// Synchronized output brackets (DEC private mode 2026): the terminal buffers
// everything between them and applies it as one frame. Terminals without the
// mode ignore the markers.
const (
	syncBegin = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
)
