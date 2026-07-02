package screen

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Overlay composites over onto base at column x, row y (both zero-based) and
// returns the resulting lines. It is how dialogs, pickers, and panels appear:
// the base component renders as usual, the overlay is spliced over it before
// the frame is painted, and dismissing the overlay simply stops splicing.
// Nothing about the base changes, so overlays never corrupt underlying state.
//
// The splice is width-aware (grapheme clusters, escape sequences), and the
// overlay content is isolated from the base row's styling with an SGR reset
// on both sides. The base row's own styling does not resume after the
// overlay: re-opening an arbitrary interrupted style run is not worth the
// complexity when overlays are opaque boxes that cover the styled content
// anyway.
//
// Rows and columns beyond the base are padded with blanks, so an overlay can
// extend past the base's bottom edge (a dialog taller than the live region
// grows the frame rather than being clipped).
func Overlay(base, over []string, x, y int) []string {
	if len(over) == 0 {
		return base
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	rows := len(base)
	if need := y + len(over); need > rows {
		rows = need
	}
	out := make([]string, rows)
	copy(out, base)
	for i, ov := range over {
		row := y + i
		ln := out[row]
		left := ansi.Truncate(ln, x, "")
		if pad := x - ansi.StringWidth(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		right := ansi.TruncateLeft(ln, x+ansi.StringWidth(ov), "")
		out[row] = left + sgrReset + ov + sgrReset + right
	}
	return out
}

// Center returns the x, y position that centers a box of the given size
// within a frame of the given size, clamped to the top-left when the box is
// larger than the frame.
func Center(frameWidth, frameHeight, boxWidth, boxHeight int) (x, y int) {
	x = (frameWidth - boxWidth) / 2
	y = (frameHeight - boxHeight) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return x, y
}

const sgrReset = "\x1b[0m"
