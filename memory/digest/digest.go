// Package digest builds the wake digest: the short, token-budgeted list of what
// the agent already knows, handed to a session at wake without anybody having
// asked for it.
//
// It exists because retrieval on its own is pull-only. A recall requires the
// reader to already suspect the fact exists, so a standing preference the operator
// stated last month is only honoured by a session that thinks to go looking for
// it. The digest is the push half: one line per item, present in every session's
// opening context, cheap enough to afford on every wake.
//
// Bodies stay behind recall. A line carries the item's id, its kind and its first
// sentence, which is enough for a reader to know the fact exists and to ask for the
// rest by id. Putting whole bodies in the digest would spend the entire budget on a
// handful of items and make the push channel the primary read path, which is the
// opposite of what it is for.
//
// Two properties are load-bearing and easy to lose:
//
//   - Only what guard.PushEligibility admits ever reaches a line. The digest is the
//     one path that reaches every reader unasked, so one poisoned line in it is
//     persistent prompt injection into every session for as long as the item lives.
//     Selection runs through guard.Pushable; there is no way to build a digest that
//     skips it.
//   - Every inclusion is counted. A push is recorded against the item and marked on
//     the run's prime scope (see memory/ridealong), so a later use of a pushed item
//     is attributed as primed rather than as the reader having found it. Without
//     that split the decay signal degrades into a record of what the digest keeps
//     pushing.
//
// Selection is deliberately static in v1. Most-specific scope first, widening out
// through the ancestor chain, most recent first within a level, plus an exploration
// quota so the same handful of items cannot own the digest forever. Learned ranking
// waits until the usage counters carry real data; ranking on a signal the push
// itself produces is rich-get-richer with extra steps.
//
// The one thing the counters already say without being asked to predict anything is
// which items the push is wrong about. An item pushed at every wake and never once
// used is the digest spending budget on itself, and it is demoted: ranked behind
// everything else, and passed over by the exploration reserve. Demotion is a ranking
// and not an exclusion, so the item keeps whatever budget is left and one use ends it
// (Builder.WithDemoteAfter).
//
// Host neutrality. Nothing here names a host concept. A digest is built from a
// state.RecallQuery and returns lines of text; where those lines go, and what frames
// them in a prompt, is the host's to decide.
package digest

import (
	"strings"
	"unicode"

	"github.com/ionalpha/flynn/state"
)

// charsPerToken is the rough bytes-per-token ratio the budget is counted in. It is
// a deliberate approximation: the digest is budgeted so it cannot crowd out the
// session's actual work, and that job is served by an estimate that never needs a
// tokenizer, a model name, or a network call. A host holding a real tokenizer sets
// the budget lower to buy margin rather than replacing the estimate here.
const charsPerToken = 4

// Line is one item's entry in the digest: enough to know the fact exists and to
// ask for the rest of it, and no more.
type Line struct {
	// MemoryID is the item this line summarizes, and the handle a reader passes back
	// to recall the full body. It is the only part of the line that has to be exact.
	MemoryID string
	// Kind is the item's kind, carried so a reader can tell a stated preference from
	// an observation without reading the sentence twice.
	Kind string
	// Scope is where the item was written. It is what the ordering is built on, and
	// it tells a reader whether a fact is about this workspace or inherited from
	// something wider.
	Scope state.Scope
	// Summary is the item's first sentence, whitespace collapsed and length-capped.
	// It is a lossy rendering of the content on purpose (see Summarize).
	Summary string
	// Tokens is the estimated cost of this line as rendered, including its newline.
	// It is set by the builder and is what the budget is spent against.
	Tokens int
	// Demoted reports that this item has been pushed repeatedly and never used, so it
	// was ranked behind everything still earning its place
	// (Builder.WithDemoteAfter). It is carried out to the caller because it is the
	// only place the selection says which items the fleet is ignoring, and that is a
	// curator's question; a demoted line still reads to a session as any other line.
	Demoted bool
}

// Text renders the line as it appears in the digest: the id first, because that is
// what a reader has to copy back to recall the body, then the kind, then the
// sentence.
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

// Digest is a selected, budgeted set of lines plus the accounting behind it.
type Digest struct {
	// Lines are the selected entries, in the order they should be read: the
	// selection order, not the order the passes chose them in (see Builder.Select).
	Lines []Line
	// Budget is the token budget the selection was made against.
	Budget int
	// Tokens is the estimated cost of Lines, always <= Budget.
	Tokens int
	// Dropped are the eligible lines that did not fit, in the order they were
	// considered. It is the record of what the budget cost, kept as data rather than
	// written to a log: a library that logged this would pick the host's log format,
	// its level and its sink, and a host that wants none of it could not opt out. A
	// host that wants it logged ranges over this and logs it.
	Dropped []Line
	// Considered is how many push-eligible items the selection chose between, which
	// is len(Lines) + len(Dropped).
	Considered int
	// Capped reports that the candidate read hit its own limit
	// (Builder.WithCandidateLimit), so items the selection never saw may outrank what
	// it chose. It is stated rather than left implicit because a truncated search that
	// reports nothing reads exactly like a complete one.
	Capped bool
}

// Text renders the digest as the lines a host injects, one per line and no
// trailing newline. There is no header and no framing: what introduces the digest
// in a prompt is the host's wording, and a library that supplied it would be
// writing half of somebody else's system prompt.
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

// IDs returns the ids of the selected lines, in line order. It is what a push is
// recorded against, and what a host replaying an already-built digest marks on the
// prime scope.
func (d Digest) IDs() []string {
	out := make([]string, 0, len(d.Lines))
	for _, l := range d.Lines {
		out = append(out, l.MemoryID)
	}
	return out
}

// minSentence is the shortest prefix Summarize will accept as a whole sentence. It
// exists so an abbreviation near the start of the content ("e.g.", "cf.", a version
// number) does not end the sentence one word in and leave a line that says nothing.
// It costs the genuinely short sentence its terminator, which is a rendering detail,
// where the other failure loses the whole meaning.
const minSentence = 24

// Summarize reduces content to a single line: whitespace collapsed to single
// spaces, cut at the end of the first sentence, and capped at maxChars with an
// ellipsis when it is still too long. An empty or all-whitespace content yields "".
//
// It is deliberately lossy and deliberately not a model call. The line is an index
// entry whose job is to let a reader decide whether to recall the body; a summary
// good enough to act on without the body would make the digest the read path.
//
// A sentence ends at ".", "!" or "?" followed by a space or the end of the content,
// at least minSentence characters in. Abbreviations later in a long first sentence
// will still cut it short, which costs a truncated line and never a wrong id.
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

// firstSentence returns s up to and including its first sentence terminator, or all
// of s when it has none. s is already whitespace-collapsed.
func firstSentence(s string) string {
	for i, r := range s {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if i+1 < minSentence {
			continue
		}
		// The rune after the terminator decides whether it ended a sentence: a space
		// or the end of the content does, anything else (a decimal point, a path, an
		// ellipsis mid-string) does not.
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

// truncate caps s at maxChars, cutting at the last word boundary that leaves room
// for the ellipsis so a line never ends mid-word. A single word longer than the cap
// is cut where it falls, because there is no boundary to find.
func truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	const ellipsis = "..."
	if maxChars <= len(ellipsis) {
		return s[:maxChars]
	}
	cut := maxChars - len(ellipsis)
	// Back up off a partial rune, then to the last space, so the cut lands on a word
	// boundary and never inside a multi-byte character.
	for cut > 0 && !isBoundary(s[cut]) {
		cut--
	}
	trimmed := strings.TrimRight(s[:cut], " ")
	if trimmed == "" {
		// One long word: cut it where the budget ran out, backing off any partial rune.
		cut = maxChars - len(ellipsis)
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		trimmed = s[:cut]
	}
	return trimmed + ellipsis
}

// isBoundary reports whether the byte at a candidate cut is a space, which is both
// a word boundary and, being ASCII, always a rune boundary.
func isBoundary(b byte) bool { return b == ' ' }

// utf8Start reports whether b begins a UTF-8 rune rather than continuing one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// estimateTokens approximates the token cost of a rendered line, rounding up so a
// line never costs nothing, and charging one character for the newline that
// separates it from the next.
func estimateTokens(s string) int {
	return (len(s) + 1 + charsPerToken - 1) / charsPerToken
}

// hasContent reports whether a string carries anything a reader could read. It
// keeps an item whose content is only punctuation or whitespace out of the digest,
// where it would spend budget on a line that says nothing.
func hasContent(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
