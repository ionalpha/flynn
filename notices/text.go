package notices

import (
	"strings"
	"unicode/utf8"
)

// Sanitize turns a string that arrived over the network into a string that is safe to
// hand a terminal, truncated to at most maxRunes.
//
// Text from the feed is printed to a terminal, and a terminal is an interpreter. An
// escape sequence in a notice could rewrite lines that were already printed (so a notice
// could overwrite the governance decisions above it), set the window title, or on some
// terminals stuff characters into the input buffer, which is a command-execution
// primitive. Signing the feed does not help here: the escape would be authentically
// signed by us. So the text is stripped before it can ever be printed, and the stripping
// happens once, at decode, rather than at each print site.
//
// The rule is a deny-by-default one: keep printable runes and nothing else. That drops
// ESC and the rest of the C0 controls, the C1 controls (including the 8-bit CSI and OSC
// introducers, which a naive "strip \x1b[" filter misses entirely), DEL, and invalid
// UTF-8. Newlines and tabs are the deliberate exception, allowed in Detail because a
// paragraph of advisory text needs them and neither carries an escape.
func Sanitize(s string, maxRunes int) string {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			break
		}
		if !printable(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// printable reports whether r may reach a terminal.
func printable(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		// The only control characters a notice may carry: they lay out text and cannot
		// begin an escape sequence.
		return true
	case r == utf8.RuneError:
		// Invalid UTF-8 decodes to this. Dropping it means a feed cannot smuggle bytes
		// through a terminal's own decoder.
		return false
	case r < 0x20 || r == 0x7f:
		// C0 controls and DEL. ESC (0x1b) lives here, so this one line is what stops
		// CSI and OSC sequences in their 7-bit form.
		return false
	case r >= 0x80 && r <= 0x9f:
		// C1 controls. 0x9b is CSI and 0x9d is OSC in their 8-bit form, which terminals
		// in a UTF-8 locale generally do not honour but some do; there is no legitimate
		// reason for a notice to contain one either way.
		return false
	default:
		return true
	}
}
