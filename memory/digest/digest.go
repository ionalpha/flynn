// Package digest builds the wake digest: a token-budgeted list of what the agent
// knows, given to every session unasked. Recall only finds what a reader suspects
// exists, so a standing preference needs pushing.
//
// A line carries the item's id, kind and first sentence; the body stays behind
// recall, so a few items cannot spend the whole budget.
//
// Two invariants:
//
//   - Selection runs through guard.Pushable, with no way around it, because one
//     poisoned line is prompt injection into every session.
//   - Every inclusion is recorded as a push and marked on the run's prime scope (see
//     memory/ridealong), so a later use counts as primed and the decay signal does
//     not just echo what the digest pushes.
//
// Selection is static: most specific scope first, newest first within a level,
// plus an exploration quota. Ranking on a signal the push produces would feed
// itself. An item pushed repeatedly and never used is demoted, ranked last and
// skipped by exploration, and one use clears it ([WithDemoteAfter]).
//
// The host decides where the lines go and how a prompt frames them.
package digest

import (
	"strings"
	"unicode"

	"github.com/ionalpha/flynn/state"
)

// charsPerToken is an estimate that needs no tokenizer. A host wanting margin sets
// a lower budget.
const charsPerToken = 4

// Line is one item's entry in the digest.
type Line struct {
	// MemoryID is the handle a reader passes to recall the body.
	MemoryID string
	// Kind is the item's kind.
	Kind string
	// Scope is where the item was written, which orders the selection.
	Scope state.Scope
	// Summary is the first sentence (see [Summarize]).
	Summary string
	// Tokens is the estimated cost of the rendered line, newline included.
	Tokens int
	// Demoted reports an item pushed repeatedly and never used, for a curator. It
	// renders like any other line.
	Demoted bool
}

// Text renders the id first, because a reader copies it back, then kind and sentence.
func (l Line) Text() string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(l.MemoryID)
	if l.Kind != "" {
		b.WriteString(" [")
		b.WriteString(l.Kind)
		b.WriteByte(']')
	}
	if l.Summary != "" {
		b.WriteString(": ")
		b.WriteString(l.Summary)
	}
	return b.String()
}

// Digest is the selected lines and their accounting.
type Digest struct {
	// Lines are in reading order (see [Builder.Select]).
	Lines []Line
	// Budget is the token budget.
	Budget int
	// Tokens is the cost of Lines, at most Budget.
	Tokens int
	// Dropped are eligible lines that did not fit, in order considered, returned as
	// data so the host chooses whether to log them.
	Dropped []Line
	// Considered is len(Lines) + len(Dropped).
	Considered int
	// Capped reports the candidate read hit [WithCandidateLimit], so unseen
	// items may outrank the chosen ones.
	Capped bool
}

// Text joins the lines with newlines, with no header, because the host frames the
// prompt.
func (d Digest) Text() string {
	if len(d.Lines) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d.Lines))
	for _, l := range d.Lines {
		parts = append(parts, l.Text())
	}
	return strings.Join(parts, "\n")
}

// IDs returns the line ids in order, for recording pushes and marking the prime
// scope.
func (d Digest) IDs() []string {
	out := make([]string, 0, len(d.Lines))
	for _, l := range d.Lines {
		out = append(out, l.MemoryID)
	}
	return out
}

// minSentence is the shortest first sentence accepted, so an early "e.g." or
// version number does not end it one word in. A short sentence runs on instead.
const minSentence = 24

// Summarize collapses whitespace, cuts at the first sentence end and caps at
// maxChars with "...". Blank content yields "". It is lossy so the line indexes the
// body and does not replace it.
//
// A sentence ends at ".", "!" or "?" before a space or the end, at least
// [minSentence] characters in. A later abbreviation can still cut it short.
func Summarize(content string, maxChars int) string {
	s := strings.Join(strings.Fields(content), " ")
	if s == "" {
		return ""
	}
	s = firstSentence(s)
	if maxChars > 0 {
		s = truncate(s, maxChars)
	}
	return s
}

// firstSentence expects s whitespace-collapsed.
func firstSentence(s string) string {
	for i, r := range s {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if i+1 < minSentence {
			continue
		}
		// Only a space or the end follows a sentence; a decimal point or path does not.
		rest := s[i+len(string(r)):]
		if rest == "" {
			return s
		}
		if rest[0] == ' ' {
			return s[:i+len(string(r))]
		}
	}
	return s
}

// truncate cuts at the last space that leaves room for "...". A single word over
// the cap is cut where it falls.
func truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	const ellipsis = "..."
	if maxChars <= len(ellipsis) {
		return s[:maxChars]
	}
	cut := maxChars - len(ellipsis)
	for cut > 0 && !isBoundary(s[cut]) {
		cut--
	}
	trimmed := strings.TrimRight(s[:cut], " ")
	if trimmed == "" {
		// Back off a partial rune.
		cut = maxChars - len(ellipsis)
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		trimmed = s[:cut]
	}
	return trimmed + ellipsis
}

// isBoundary reports a space, which is ASCII and so also a rune boundary.
func isBoundary(b byte) bool { return b == ' ' }

// utf8Start reports whether b begins a UTF-8 rune rather than continuing one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// estimateTokens rounds up and charges one character for the newline.
func estimateTokens(s string) int {
	return (len(s) + 1 + charsPerToken - 1) / charsPerToken
}

// hasContent reports a letter or digit, keeping punctuation-only items out.
func hasContent(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
