// Package inertext makes text from outside the agent safe to show a person. The
// words in an approval request (an action, a scope, a host) can come from a model,
// a tool or a peer, and a terminal or log viewer acts on control bytes: an escape
// sequence repaints a line, a carriage return overwrites, and a bidirectional
// override reorders characters. Text passed through [Escape] does none of that, and
// the reader can see something was there.
package inertext

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Escape replaces every character a terminal or renderer would act on with a
// visible escape: \x1b for a control byte, \u202e for a bidirectional override or
// other invisible format character, \xNN for invalid UTF-8. Tabs, newlines and
// ordinary characters, backslash included, pass unchanged.
//
// The output contains none of the rewritten characters, so escaping twice changes
// nothing, and a reader can apply it to a stored record without knowing whether the
// writer did.
func Escape(s string) string {
	if !needsEscape(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(`\x`)
			b.WriteString(hex(uint64(s[i]), 2))
		case inert(r):
			b.WriteRune(r)
		case r < 0x100:
			b.WriteString(`\x`)
			b.WriteString(hex(uint64(r), 2)) //nolint:gosec // DecodeRuneInString never returns a negative rune
		case r < 0x10000:
			b.WriteString(`\u`)
			b.WriteString(hex(uint64(r), 4))
		default:
			b.WriteString(`\U`)
			b.WriteString(hex(uint64(r), 8))
		}
		i += size
	}
	return b.String()
}

// Limit returns the longest prefix of s within limit bytes ending on a character
// boundary, so the cut is valid UTF-8. Apply it after [Escape]: a cut inside an
// escape such as \x1b leaves plain characters.
func Limit(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func needsEscape(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || !inert(r) {
			return true
		}
		i += size
	}
	return false
}

// inert reports whether r is shown as itself. Tab and newline stay, because a
// multi-line scope is ordinary; other control characters and invisible format
// characters (zero-width joiners, bidirectional marks, the byte order mark) do not.
func inert(r rune) bool {
	switch r {
	case '\t', '\n':
		return true
	}
	return !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && r != ' ' && r != ' '
}

func hex(v uint64, width int) string {
	h := strconv.FormatUint(v, 16)
	if len(h) < width {
		h = strings.Repeat("0", width-len(h)) + h
	}
	return h
}
