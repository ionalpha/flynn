// Package screen is the terminal renderer core for the interactive session:
// line-array components, a diffing painter and scrollback-native output.
//
// A component renders itself to pre-styled lines at a width; components compose
// by concatenation and overlays composite over the base lines before painting.
// The painter owns only a small live region at the bottom of the terminal
// (in-flight output, status line, composer) and rewrites only the rows that
// changed. Finalized content is printed above it as ordinary lines, so native
// scrollback, selection, copy and search keep working on it. Those lines are
// rewritten only by Reflow, when a width change means they must be laid out
// again.
//
// Every paint is wrapped in synchronized output (DEC private mode 2026) so a
// repaint is one frame. Terminals without the mode ignore the markers and still
// receive each frame as one write.
//
// The renderer reads nothing back and tracks no cursor position beyond the live
// region it last painted, so the core is testable as bytes in, bytes
// out, with no terminal.
package screen

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// Surface is the render target the shell drives. Painter renders inline and
// commits finalized lines to the terminal's scrollback; it is the default.
// AltPainter renders in the alternate screen, with its own bounded scrollback,
// for emulators where inline scroll-region insertion is unsafe (Zellij-class
// multiplexers). Every method is a no-op after the first write error, which Err
// reports.
type Surface interface {
	// Resize records a new terminal size; the next paint re-renders at it.
	Resize(width, height int)
	// Paint shows frame as the live region, writing only what changed.
	Paint(frame []string)
	// Insert commits finalized above the live region, then shows frame.
	Insert(finalized, frame []string)
	// Reflow replaces everything committed so far with finalized, then shows
	// frame. It is for a width change, which leaves earlier output wrapped for a
	// window that no longer exists.
	Reflow(finalized, frame []string)
	// Repaint redraws frame from scratch, discarding any stale diff base.
	Repaint(frame []string)
	// Close finalizes the session's last frame and releases the surface.
	Close()
	// Err returns the first write error the surface hit, or nil.
	Err() error
}

// Component renders itself as lines at a width. Each string must be one styled,
// already wrapped terminal row: the painter truncates overflow.
type Component interface {
	Render(width int) []string
}

// Func adapts a function to a [Component].
type Func func(width int) []string

// Render calls f.
func (f Func) Render(width int) []string { return f(width) }

// Width returns the terminal cells s occupies, counting grapheme clusters and
// skipping escape sequences.
func Width(s string) int { return ansi.StringWidth(s) }

// Truncate cuts s to width cells, keeping escape sequences and whole grapheme
// clusters.
func Truncate(s string, width int) string { return ansi.Truncate(s, width, "") }

// Wrap word-wraps plain text to width. Rows break at spaces and newlines only, so
// a hyphenated word, path or flag that fits on a row stays whole; a word wider than
// a row is broken between grapheme clusters. Indentation and the spacing between
// words on one row are kept. A width below one leaves s as it is.
func Wrap(s string, width int) string {
	if width < 1 {
		return s
	}
	var rows []string
	for _, line := range strings.Split(s, "\n") {
		rows = wrapLine(rows, line, width)
	}
	return strings.Join(rows, "\n")
}

// wrapLine appends the rows of one line, which holds no newline, to rows.
func wrapLine(rows []string, line string, width int) []string {
	row := ""
	for line != "" {
		word := strings.TrimLeft(line, " ")
		gap := line[:len(line)-len(word)]
		line = ""
		if i := strings.IndexByte(word, ' '); i >= 0 {
			word, line = word[:i], word[i:]
		}
		if word == "" {
			break // trailing spaces
		}
		if next := row + gap + word; Width(next) <= width {
			row = next
			continue
		}
		if row != "" {
			rows = append(rows, row)
		}
		if Width(word) <= width {
			row = word
			continue
		}
		row = ""
		for g := uniseg.NewGraphemes(word); g.Next(); {
			if row != "" && Width(row+g.Str()) > width {
				rows = append(rows, row)
				row = ""
			}
			row += g.Str()
		}
	}
	return append(rows, row)
}
