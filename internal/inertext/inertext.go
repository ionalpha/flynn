// Package inertext makes text that came from outside the agent safe to show a person.
// The words in an approval request (an action name, a scope, a host) can be chosen by a
// model, a tool or a peer, and a terminal or a log viewer interprets control bytes: an
// escape sequence moves the cursor and repaints a line, a carriage return overwrites what
// came before it, and a bidirectional override reorders the characters around it. Text
// that went through [Escape] cannot do any of that, and the person reading it can see
// that something was there.
package inertext

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Escape returns s with every character that a terminal or a text renderer would act on
// replaced by a visible escape: \x1b for a control byte, \u202e for a bidirectional
// override or another invisible format character, and \xNN for a byte that is not valid
// UTF-8. Tabs, newlines and every ordinary character, including a backslash, pass
// through unchanged.
//
// The output contains none of the characters it rewrites, so escaping it again changes
// nothing. That is what lets a reader apply it to a stored record without asking whether
// the writer already did.
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

// Limit returns the longest prefix of s that is at most limit bytes and ends on a character
// boundary, so the cut never produces invalid UTF-8. Apply it to text that [Escape]
// already handled: a cut can end inside a visible escape such as \x1b, and what is left
// is still plain characters.
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

// inert reports whether r is shown as itself. Tab and newline are kept because a
// multi-line scope is ordinary; every other control character, and every invisible format
// character (zero-width joiners, bidirectional marks and overrides, the byte order mark),
// is not.
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
