package screen_test

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/screen"
)

// vt is a miniature virtual terminal: it interprets exactly the escape
// vocabulary the painter emits (relative cursor movement, erase line right,
// erase display below, carriage return, line feed, the synchronized-output
// markers) over an unbounded scrollback. It exists to state the painter's
// contract as an end-to-end property: whatever mix of diffed paints,
// insertions, and repaints ran, the terminal must show the committed lines
// followed by the current frame, exactly.
type vt struct {
	rows []string
	row  int
	col  int
}

func (v *vt) write(s string) {
	i := 0
	for i < len(s) {
		switch c := s[i]; c {
		case 0x1b:
			i += v.escape(s[i:])
		case '\r':
			v.col = 0
			i++
		case '\n':
			v.row++
			i++
		default:
			v.putByte(c)
			i++
		}
	}
}

// escape interprets one escape sequence and returns its byte length.
func (v *vt) escape(s string) int {
	// All painter sequences are CSI: ESC [ params final.
	if len(s) < 3 || s[1] != '[' {
		panic("vt: non-CSI escape emitted by the painter: " + strconv.Quote(s))
	}
	j := 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j == len(s) {
		panic("vt: unterminated escape: " + strconv.Quote(s))
	}
	params, final := s[2:j], s[j]
	n := 1
	if p := strings.TrimPrefix(params, "?"); p != params {
		// A private mode (synchronized output): display-neutral.
		return j + 1
	}
	if params != "" {
		var err error
		if n, err = strconv.Atoi(params); err != nil {
			panic("vt: unexpected params " + strconv.Quote(params))
		}
	}
	switch final {
	case 'A':
		v.row -= n
		if v.row < 0 {
			panic("vt: cursor moved above the screen")
		}
	case 'B':
		v.row += n
	case 'K': // erase to end of line
		if v.row < len(v.rows) && v.col <= len(v.rows[v.row]) {
			v.rows[v.row] = v.rows[v.row][:v.col]
		}
	case 'J': // erase from cursor to end of screen
		if v.row < len(v.rows) {
			if v.col <= len(v.rows[v.row]) {
				v.rows[v.row] = v.rows[v.row][:v.col]
			}
			for r := v.row + 1; r < len(v.rows); r++ {
				v.rows[r] = ""
			}
		}
	default:
		panic("vt: unexpected final byte " + string(final))
	}
	return j + 1
}

func (v *vt) putByte(c byte) {
	for v.row >= len(v.rows) {
		v.rows = append(v.rows, "")
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

// screen returns the terminal content with trailing blank rows trimmed.
func (v *vt) screen() []string {
	end := len(v.rows)
	for end > 0 && v.rows[end-1] == "" {
		end--
	}
	return v.rows[:end]
}

// genLine draws one plain frame line that the guard passes through unchanged
// (ASCII, within width), so the property compares against the frame as-is.
func genLine(rt *rapid.T, width int) string {
	return rapid.StringMatching(`[ -~]{0,`+strconv.Itoa(width)+`}`).Draw(rt, "line")
}

func genFrame(rt *rapid.T, width, maxRows int) []string {
	n := rapid.IntRange(0, maxRows).Draw(rt, "rows")
	frame := make([]string, n)
	for i := range frame {
		frame[i] = genLine(rt, width)
	}
	return frame
}

// TestProp_PainterConvergesToTheFrame is the renderer's core contract: after
// any sequence of diffed paints, scrollback insertions, and full repaints,
// a terminal interpreting the painter's output shows exactly the committed
// lines followed by the last frame. The diff optimization is therefore
// unobservable: skipping unchanged rows can never leave a stale cell behind.
func TestProp_PainterConvergesToTheFrame(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		const width, height = 30, 8
		term := &vt{}
		var out bytes.Buffer
		p := screen.NewPainter(&out, width, height)

		var committed []string
		var frame []string
		steps := rapid.IntRange(1, 12).Draw(rt, "steps")
		for range steps {
			switch rapid.IntRange(0, 3).Draw(rt, "op") {
			case 0, 1: // Paint dominates real usage
				frame = genFrame(rt, width, height-1)
				p.Paint(frame)
			case 2:
				finalized := genFrame(rt, width, 4)
				frame = genFrame(rt, width, height-1)
				p.Insert(finalized, frame)
				committed = append(committed, finalized...)
			case 3:
				p.Repaint(frame)
			}
			if p.Err() != nil {
				rt.Fatalf("painter error: %v", p.Err())
			}
			term.write(out.String())
			out.Reset()

			want := trimTrailingBlank(append(append([]string{}, committed...), frame...))
			got := term.screen()
			if !equal(got, want) {
				rt.Fatalf("terminal state diverged\n got %q\nwant %q", got, want)
			}
		}
	})
}

func trimTrailingBlank(lines []string) []string {
	end := len(lines)
	for end > 0 && lines[end-1] == "" {
		end--
	}
	return lines[:end]
}

func equal(a, b []string) bool {
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

// TestProp_IdenticalFramePaintsNothing pins the diff's efficiency contract:
// re-painting any frame writes zero bytes.
func TestProp_IdenticalFramePaintsNothing(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		const width, height = 30, 8
		var out bytes.Buffer
		p := screen.NewPainter(&out, width, height)
		frame := genFrame(rt, width, height-1)
		p.Paint(frame)
		out.Reset()
		p.Paint(append([]string{}, frame...))
		if out.Len() != 0 {
			rt.Fatalf("identical frame wrote %q", out.String())
		}
	})
}

// TestProp_GuardBoundsEveryLiveRow: whatever a component renders (any width,
// any row count, escapes, wide glyphs), every live row fits the terminal.
func TestProp_GuardBoundsEveryLiveRow(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		width := rapid.IntRange(1, 40).Draw(rt, "width")
		height := rapid.IntRange(2, 12).Draw(rt, "height")
		var out bytes.Buffer
		p := screen.NewPainter(&out, width, height)
		n := rapid.IntRange(0, 20).Draw(rt, "rows")
		frame := make([]string, n)
		for i := range frame {
			frame[i] = rapid.String().Draw(rt, "line")
		}
		p.Paint(frame)
		live := p.Live()
		if len(live) > height-1 {
			rt.Fatalf("live region %d rows exceeds the height cap %d", len(live), height-1)
		}
		for _, ln := range live {
			if screen.Width(ln) > width {
				rt.Fatalf("live row %q is %d cells wide at width %d", ln, screen.Width(ln), width)
			}
		}
	})
}
