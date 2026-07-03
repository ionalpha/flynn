package screen

import (
	"bytes"
	"io"

	"github.com/charmbracelet/x/ansi"
)

// maxAltHistory bounds the scrollback AltPainter retains. The alternate screen
// buffer has no scrollback of its own, so the painter keeps the recent
// transcript it might still need to draw and lets older lines fall out of
// memory as they scroll past the top of what the viewport can show.
const maxAltHistory = 5000

// AltPainter renders the session into the terminal's alternate screen: a
// fixed-size viewport that it repaints in full each frame with absolute cursor
// addressing. It is the fallback for emulators where the inline Painter's
// scroll-region insertion is unsafe (Zellij-class multiplexers), so it cannot
// lean on the terminal's own scrollback the way Painter does; instead it keeps
// a bounded transcript of its own and always draws the tail, the newest output
// with the composer beneath it. Entering and leaving the alternate screen is
// the term package's job, symmetric with the other terminal modes; this type
// only draws inside it.
//
// Like Painter it is pure with respect to the terminal (it reads nothing back)
// and every write is one synchronized frame, so a repaint never tears. Write
// errors are sticky: after the first failure every later operation is a no-op
// and Err reports the cause.
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

// NewAltPainter builds an alternate-screen painter over w for a viewport of the
// given size. The caller has already entered the alternate screen (through the
// term package), so the buffer starts blank; the first paint fills it.
func NewAltPainter(w io.Writer, width, height int) *AltPainter {
	p := &AltPainter{w: w}
	p.Resize(width, height)
	return p
}

// Err returns the first write error the painter hit, or nil.
func (p *AltPainter) Err() error { return p.err }

// Live returns the live-region rows currently composed. It exists for tests
// and diagnostics; callers must not mutate the returned slice.
func (p *AltPainter) Live() []string { return p.live }

// Resize records a new viewport size. The previously drawn rows can no longer
// be trusted as a diff base after the terminal reflowed them, so the next paint
// clears the viewport and redraws it in full.
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

// Paint shows frame as the live region and repaints the viewport, writing only
// the rows that changed since the last frame.
func (p *AltPainter) Paint(frame []string) {
	p.live = frame
	p.render()
}

// Insert commits finalized into the painter's own scrollback above the live
// region, then shows frame. Unlike the inline painter the finalized lines do
// not become native terminal output (the alternate screen has no scrollback):
// they join the retained transcript and scroll off the top as newer output
// arrives, bounded by maxAltHistory.
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

// Repaint shows frame and redraws the whole viewport from scratch, discarding
// the diff base. It is the recovery path after a resize or after output from
// outside the painter.
func (p *AltPainter) Repaint(frame []string) {
	p.live = frame
	p.dirty = true
	p.render()
}

// Close releases the painter. The alternate screen and everything drawn in it
// is discarded when the caller leaves it (the term package's teardown), so
// there is nothing to leave behind; Close exists to satisfy the Surface
// contract and is safe to call once.
func (p *AltPainter) Close() {}

// render draws the current viewport. An unchanged frame writes nothing; a
// changed one writes only its changed rows, all inside one synchronized frame.
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
		// The viewport is untrusted (first paint, resize, or recovery): clear it
		// and treat every row as needing a write against a blank base.
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

// viewport composes the height rows to draw: the tail of the transcript
// followed by the live region, each truncated to one row, top-aligned so the
// composer sits directly under the newest output and reaches the bottom edge
// only once the screen fills. Rows past the end of the content stay blank.
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
