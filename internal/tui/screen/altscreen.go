package screen

import (
	"bytes"
	"io"

	"github.com/charmbracelet/x/ansi"
)

// maxAltHistory bounds the transcript AltPainter retains, since the alternate
// screen has no scrollback.
const maxAltHistory = 5000

// AltPainter renders into the alternate screen: a fixed-size viewport repainted
// with absolute cursor addressing. It is the fallback where the inline
// Painter's scroll-region insertion is unsafe (Zellij-class multiplexers), so it
// keeps its own bounded transcript and draws the tail: the newest output with
// the composer beneath. The term package enters and leaves the alternate screen.
//
// It reads nothing back and writes one synchronized frame per repaint. Write
// errors are sticky: after the first, every operation is a no-op and Err reports
// it.
type AltPainter struct {
	w       io.Writer
	width   int
	height  int
	history []string // committed transcript, bounded to maxAltHistory
	live    []string // the live region, drawn at the bottom of the viewport
	shown   []string // the rows currently on screen, one entry per viewport row
	dirty   bool     // the next paint must clear and redraw the whole viewport
	err     error
}

// NewAltPainter builds a painter over w. The caller has already entered the
// alternate screen, so the buffer starts blank.
func NewAltPainter(w io.Writer, width, height int) *AltPainter {
	p := &AltPainter{w: w}
	p.Resize(width, height)
	return p
}

// Err returns the first write error the painter hit, or nil.
func (p *AltPainter) Err() error { return p.err }

// Live returns the composed live rows, for tests and diagnostics. Callers must
// not mutate the slice.
func (p *AltPainter) Live() []string { return p.live }

// Resize records a new viewport size. The terminal reflowed the drawn rows, so
// the next paint redraws in full.
func (p *AltPainter) Resize(width, height int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	p.width, p.height = width, height
	p.dirty = true
}

// Paint draws frame over the alternate screen.
func (p *AltPainter) Paint(frame []string) {
	p.live = frame
	p.render()
}

// Insert appends finalized to the retained transcript, bounded by maxAltHistory,
// then shows frame. Unlike the inline painter these lines never become native
// terminal output.
func (p *AltPainter) Insert(finalized, frame []string) {
	if len(finalized) > 0 {
		p.history = append(p.history, finalized...)
		if over := len(p.history) - maxAltHistory; over > 0 {
			p.history = append(p.history[:0], p.history[over:]...)
		}
	}
	p.live = frame
	p.render()
}

// Repaint redraws the whole viewport without diffing, to recover after a resize
// or output from outside the painter.
func (p *AltPainter) Repaint(frame []string) {
	p.live = frame
	p.dirty = true
	p.render()
}

// Close satisfies [Surface]. Leaving the alternate screen discards everything
// drawn, so there is nothing to release.
func (p *AltPainter) Close() {}

func (p *AltPainter) render() {
	if p.err != nil {
		return
	}
	want := p.viewport()
	if !p.dirty && equalLines(p.shown, want) {
		return
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	if p.dirty {
		// Untrusted viewport (first paint, resize, recovery): clear and rewrite every row.
		b.WriteString(ansi.EraseDisplay(2))
		p.shown = make([]string, p.height)
		p.dirty = false
	}
	for i, row := range want {
		if i < len(p.shown) && row == p.shown[i] {
			continue
		}
		b.WriteString(ansi.CursorPosition(1, i+1))
		b.WriteString(row)
		b.WriteString(ansi.EraseLineRight)
	}
	b.WriteString(syncEnd)
	p.flush(&b)
	p.shown = want
}

// viewport composes the rows to draw: the transcript tail then the live region,
// top-aligned so the composer sits under the newest output and reaches the
// bottom only once the screen fills.
func (p *AltPainter) viewport() []string {
	rows := make([]string, p.height)
	total := len(p.history) + len(p.live)
	start := 0
	if total > p.height {
		start = total - p.height
	}
	for i := range rows {
		idx := start + i
		if idx >= total {
			break
		}
		var line string
		if idx < len(p.history) {
			line = p.history[idx]
		} else {
			line = p.live[idx-len(p.history)]
		}
		rows[i] = Truncate(line, p.width)
	}
	return rows
}

func (p *AltPainter) flush(b *bytes.Buffer) {
	if b.Len() == 0 {
		return
	}
	if _, err := p.w.Write(b.Bytes()); err != nil {
		p.err = err
	}
}
