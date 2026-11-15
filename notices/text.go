package notices

import (
	"strings"
	"unicode/utf8"
)

// Sanitize makes network text safe for a terminal and truncates it to maxRunes. An
// escape sequence could rewrite lines already printed, set the window title, or on some
// terminals inject input, and a signature does not help because we would have signed
// it. The feed decoder calls it once on every field.
//
// It keeps printable runes, newline and tab, and drops C0 and C1 controls (including
// the 8-bit CSI and OSC that a "strip \x1b[" filter misses), DEL and invalid UTF-8.
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
		return true
	case r == utf8.RuneError:
		// Invalid UTF-8, so a feed cannot pass bytes to the terminal's own decoder.
		return false
	case r < 0x20 || r == 0x7f:
		// C0 controls and DEL. ESC (0x1b) is here, which stops 7-bit CSI and OSC.
		return false
	case r >= 0x80 && r <= 0x9f:
		// C1 controls: 0x9b and 0x9d are 8-bit CSI and OSC, which some terminals
		// honour even in a UTF-8 locale.
		return false
	default:
		return true
	}
}
