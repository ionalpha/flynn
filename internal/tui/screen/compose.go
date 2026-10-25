package screen

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Overlay composites over onto base at column x, row y (both zero-based) and
// returns the resulting lines. The base renders as usual and the overlay is spliced over
// it before the frame is painted, so dismissing an overlay just stops splicing and the
// base state is never touched.
//
// The splice is width-aware (grapheme clusters, escape sequences), and the overlay is
// isolated from the base row's styling with an SGR reset on both sides. The base row's
// styling does not resume after the overlay: overlays are opaque boxes over the styled
// content, so re-opening an interrupted style run is not worth the complexity.
//
// Rows and columns beyond the base are padded with blanks, so a dialog taller than the
// live region grows the frame rather than being clipped.
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
