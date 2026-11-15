package screen

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Overlay splices over onto base at column x, row y (zero-based), leaving base
// untouched. The splice is width-aware, and an SGR reset on both sides isolates
// the overlay from the row's styling. That styling does not resume after the
// overlay: overlays are opaque boxes.
//
// Rows and columns beyond the base are padded with blanks, so a tall dialog grows
// the frame rather than being clipped.
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

// Center returns the position that centers a box in a frame, clamped to the
// top-left when the box is larger.
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
