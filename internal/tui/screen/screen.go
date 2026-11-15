// Package screen is the terminal renderer core for the interactive session:
// line-array components, a diffing painter and scrollback-native output.
//
// A component renders itself to pre-styled lines at a width; components compose
// by concatenation and overlays composite over the base lines before painting.
// The painter owns only a small live region at the bottom of the terminal
// (in-flight output, status line, composer) and rewrites only the rows that
// changed. Finalized content is printed above it as ordinary lines, so native
// scrollback, selection, copy and search keep working on it.
//
// Every paint is wrapped in synchronized output (DEC private mode 2026) so a
// repaint is one frame. Terminals without the mode ignore the markers and still
// receive each frame as one write.
//
// The renderer reads nothing back and keeps no absolute cursor position, only
// the live region it last painted, so the core is testable as bytes in, bytes
// out, with no terminal.
package screen

import "github.com/charmbracelet/x/ansi"

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

// Width returns the terminal cells s occupies, counting grapheme clusters and
// skipping escape sequences.
func Width(s string) int { return ansi.StringWidth(s) }

// Truncate cuts s to width cells, keeping escape sequences and whole grapheme
// clusters.
func Truncate(s string, width int) string { return ansi.Truncate(s, width, "") }

// Wrap word-wraps s to width, hard-breaking words wider than a line so no row
// exceeds it.
func Wrap(s string, width int) string { return ansi.Wrap(s, width, "") }
