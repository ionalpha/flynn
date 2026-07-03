// Package screen is the terminal renderer core for the interactive session:
// line-array components, a diffing painter, and scrollback-native output.
//
// The model is deliberately small. A component renders itself to a slice of
// pre-styled lines at a width; components compose by concatenation and
// overlays composite over the base lines before painting. The painter owns
// only a small live region at the bottom of the terminal (the in-flight
// output, status line, and composer) and repaints it by diffing pre-wrapped
// line arrays, rewriting only the rows that changed. Finalized content is
// printed above the live region as ordinary terminal lines, so native
// scrollback, mouse selection, copy, and terminal search all keep working on
// everything the session has already said.
//
// Every paint is wrapped in synchronized output (DEC private mode 2026), so a
// repaint is one atomic frame and never tears under fast streaming. Terminals
// that do not implement the mode ignore the markers and still receive each
// frame as a single buffered write.
//
// The renderer is pure with respect to the terminal: it reads nothing back
// and keeps no absolute cursor position, only the shape of the live region it
// last painted. That keeps the whole core testable as bytes in, bytes out,
// with no terminal required.
package screen

import "github.com/charmbracelet/x/ansi"

// Surface is the render target the shell drives: it turns frames into terminal
// writes and owns whatever region of the terminal the session paints. Two
// implementations back it. Painter renders inline and commits finalized lines
// to the terminal's own scrollback, so native scrolling, selection, and search
// keep working on the whole transcript; it is the default and the full-fidelity
// path. AltPainter renders in the alternate screen for emulators where the
// inline path's scroll-region insertion is unsafe (Zellij-class multiplexers):
// it keeps its own bounded scrollback and repaints a full-screen viewport.
//
// Both share the same contract, so the shell drives either without knowing
// which it holds. Every method is a no-op after the first write error, which
// Err reports.
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

// Component is anything that can render itself as lines at a given width.
// Each returned string is one terminal row, already styled and already
// wrapped: the painter treats every line as exactly one row and hard-guards
// overflow by truncation, so a component that wants wrapping does it here.
type Component interface {
	Render(width int) []string
}

// Width returns the number of terminal cells s occupies, counting grapheme
// clusters (emoji, CJK, combining marks) and skipping escape sequences.
func Width(s string) int { return ansi.StringWidth(s) }

// Truncate cuts s to at most width cells, preserving escape sequences and
// never splitting a grapheme cluster.
func Truncate(s string, width int) string { return ansi.Truncate(s, width, "") }

// Wrap word-wraps s to the given width, breaking on spaces and hyphens and
// falling back to a hard break for words wider than a whole line, so no
// output row can ever exceed the width.
func Wrap(s string, width int) string { return ansi.Wrap(s, width, "") }
