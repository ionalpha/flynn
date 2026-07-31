// Package guard defends the agent's durable memory against context poisoning
// (OWASP ASI06): the attack that writes a hidden instruction into memory now and
// triggers it days later, through an unrelated turn. Prompt-injection defenses on
// the live prompt do not cover it, because the payload arrives as a stored fact,
// not as the current input.
//
// The defense is structural, not a classifier, because LLM-based poison detectors
// miss most of it. Two mechanisms compose:
//
//   - Provenance-derived trust. A memory carries the trust of the source that wrote
//     it, derived as a pure function of its provenance (TrustOf). Content from an
//     external, model-reachable channel (a tool's output, an inbound message, a
//     fetched page) is untrusted; the operator's own instruction is trusted; the
//     agent's own converged run is in between. Retrieval consults this so a recalled
//     memory is treated as input carrying its source's trust, never as the agent's
//     own vetted intent.
//   - Ingest screening. Content entering memory is scanned for hidden-instruction
//     smuggling (invisible characters, bidi overrides, tag-character payloads) and
//     for overt instruction-injection phrasing (Screen). A hit from an untrusted
//     source is refused at write, before it can ever be recalled.
//
// Honest scope. The phrase screen is a soft, best-effort layer: obfuscation,
// encoding, translation, and splitting bypass it, so it raises the bar and catches
// the obvious rather than being a wall. The structural half is the invisible
// character screen (deterministic) and, above all, provenance trust: even an
// undetected payload from an untrusted source is stored, if stored at all, as
// untrusted data that the governance gate will not act on as trusted intent. The
// wall is structure; the phrase screen is a bar-raiser.
package guard

import (
	"sort"
	"strings"
	"unicode"

	"github.com/ionalpha/flynn/sandbox"
)

// Provenance scheme prefixes on a memory's Source. A host tags a write with the
// channel the content arrived through so trust is derivable without a separate
// field. Bare sources with no scheme (a run id, "chat") are the agent's own run,
// classified TrustSemi: authored by this run, not externally controlled, but not
// vetted safe either.
const (
	// SchemeUser marks content the operator authored directly: the principal's own
	// instruction. Trusted.
	SchemeUser = "user:"
	// SchemeAgent marks content the agent authored for itself (a distilled lesson,
	// a converged run's note). Semi-trusted.
	SchemeAgent = "agent:"
	// SchemeTool marks content that came out of a tool call. Untrusted: a tool's
	// output is attacker-influenceable (a fetched page, a command's stdout).
	SchemeTool = "tool:"
	// SchemeInbound marks content from an inbound channel (a received message, a
	// webhook). Untrusted.
	SchemeInbound = "inbound:"
	// SchemeWeb marks content fetched from the network. Untrusted.
	SchemeWeb = "web:"
	// SchemeExternal marks content from any other outside origin. Untrusted.
	SchemeExternal = "external:"
)

// TrustOfAll derives the trust of an item distilled from several sources: the
// weakest of them. A fact assembled from an operator's instruction and a fetched
// web page is only as vouched-for as the page, because the attacker-influenceable
// input is in the content either way, and taking the strongest source instead
// would let one trusted co-author launder everything it was mixed with.
//
// No sources at all is TrustSemi, the same answer TrustOf gives for an unlabelled
// source: an item whose provenance was never recorded is not promoted to trusted.
func TrustOfAll(sources []string) sandbox.Trust {
	if len(sources) == 0 {
		// The unlabelled answer, and the seed below would otherwise floor every
		// item at it: no recorded provenance is the agent's own run, never trusted.
		return TrustOf("")
	}
	worst := sandbox.TrustTrusted
	for _, s := range sources {
		// sandbox.Trust ascends from TrustTrusted to TrustUntrusted, so the weakest
		// source is the largest value.
		if t := TrustOf(s); t > worst {
			worst = t
		}
	}
	return worst
}

// TrustOf derives the trust of one provenance string. It is a pure function so
// retrieval and the write gate agree without storing a redundant field. The
// classification is fail-safe for the recall side: an unrecognised scheme is
// treated as the agent's own run (TrustSemi), never silently promoted to trusted,
// so a source that should have been tagged untrusted is at worst under-trusted, not
// over-trusted. See TrustOfAll for an item carrying several sources.
func TrustOf(source string) sandbox.Trust {
	switch {
	case strings.HasPrefix(source, SchemeTool),
		strings.HasPrefix(source, SchemeInbound),
		strings.HasPrefix(source, SchemeWeb),
		strings.HasPrefix(source, SchemeExternal):
		return sandbox.TrustUntrusted
	case strings.HasPrefix(source, SchemeUser):
		return sandbox.TrustTrusted
	default:
		// SchemeAgent, a bare run id, "chat", or empty: the agent's own run.
		return sandbox.TrustSemi
	}
}

// FindingKind names the class of a screening hit.
type FindingKind string

const (
	// KindInvisible is a hidden or invisible character used to smuggle instructions
	// past a human reader: a zero-width space, a soft hyphen, a byte-order mark.
	KindInvisible FindingKind = "invisible-character"
	// KindBidi is a bidirectional-override control, which can reorder displayed text
	// so what a reviewer sees differs from what is stored.
	KindBidi FindingKind = "bidi-override"
	// KindTagChars is a Unicode tag-block payload (U+E0000..U+E007F), an invisible
	// channel that encodes ASCII instructions inside otherwise-plain text.
	KindTagChars FindingKind = "tag-character-payload"
	// KindInjectionPhrase is overt instruction-injection phrasing. Soft: it catches
	// the obvious and is bypassable, documented as a bar-raiser not a wall.
	KindInjectionPhrase FindingKind = "injection-phrase"
)

// Finding is one screening hit: the class and a human-readable detail.
type Finding struct {
	Kind   FindingKind
	Detail string
}

// Structural reports whether the finding is a deterministic structural signal
// (an invisible/bidi/tag-char payload) rather than the soft phrase heuristic. The
// write gate refuses untrusted content on any finding, but structural hits are the
// part callers can trust as non-brittle.
func (f Finding) Structural() bool { return f.Kind != KindInjectionPhrase }

// injectionPhrases are overt instruction-override markers. Lower-case; matched
// case-insensitively against the content. This list is deliberately short and
// high-signal: it is a soft bar-raiser, and a long fuzzy list would add false
// positives on legitimate notes (including notes about prompt injection) without
// stopping a determined adversary, who obfuscates past any keyword set.
var injectionPhrases = []string{
	"ignore previous instructions",
	"ignore all previous instructions",
	"disregard the above",
	"disregard previous instructions",
	"forget your instructions",
	"you are now",
	"new instructions:",
	"system prompt:",
	"begin new instructions",
}

// Unicode code points used by the screens, written as explicit escapes so the
// source stays legible and never carries a literal invisible glyph itself.
const (
	softHyphen  = '\u00ad'
	zwSpace     = '\u200b'
	zwNonJoiner = '\u200c'
	zwJoiner    = '\u200d'
	wordJoiner  = '\u2060'
	bom         = '\ufeff' // byte-order mark / zero-width no-break space
	mongolianVS = '\u180e'

	bidiEmbedLo = '\u202a' // LRE through RLO (U+202E): embeddings and overrides
	bidiEmbedHi = '\u202e'
	bidiIsoLo   = '\u2066' // LRI through PDI (U+2069): isolates
	bidiIsoHi   = '\u2069'

	tagLo = '\U000e0000' // tag-character block
	tagHi = '\U000e007f'
)

// isInvisible reports whether r is a character that renders as nothing or as
// whitespace a reader would not notice, and so can hide an instruction. Ordinary
// spaces, tabs, and newlines are excluded; those are normal in stored text. The
// bidi and tag-char ranges are reported under their own kinds, so they are excluded
// here even though they are also format characters.
func isInvisible(r rune) bool {
	switch r {
	case softHyphen, zwSpace, zwNonJoiner, zwJoiner, wordJoiner, bom, mongolianVS:
		return true
	}
	if isBidi(r) || isTagChar(r) {
		return false
	}
	return unicode.Is(unicode.Cf, r)
}

func isBidi(r rune) bool {
	return (r >= bidiEmbedLo && r <= bidiEmbedHi) || (r >= bidiIsoLo && r <= bidiIsoHi)
}

func isTagChar(r rune) bool { return r >= tagLo && r <= tagHi }

// Screen scans content for hidden-instruction smuggling and overt injection
// phrasing, returning every finding in a stable order (structural kinds first,
// then phrase hits). An empty result means the content is clean by these checks;
// it is not a guarantee of safety (see the package honesty note).
func Screen(content string) []Finding {
	var invisible, bidi, tag bool
	for _, r := range content {
		switch {
		case isBidi(r):
			bidi = true
		case isTagChar(r):
			tag = true
		case isInvisible(r):
			invisible = true
		}
	}
	var out []Finding
	if invisible {
		out = append(out, Finding{KindInvisible, "content contains zero-width or invisible characters"})
	}
	if bidi {
		out = append(out, Finding{KindBidi, "content contains bidirectional-override control characters"})
	}
	if tag {
		out = append(out, Finding{KindTagChars, "content contains Unicode tag-character payload"})
	}
	lower := strings.ToLower(content)
	for _, p := range injectionPhrases {
		if strings.Contains(lower, p) {
			out = append(out, Finding{KindInjectionPhrase, "content contains instruction-override phrasing: " + p})
		}
	}
	// Stable order: structural kinds precede phrase hits; sort within each group by
	// detail for determinism.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Structural() != out[j].Structural() {
			return out[i].Structural()
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}
