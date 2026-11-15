// Package guard defends durable memory against context poisoning (OWASP ASI06): an
// instruction written into memory now and triggered later by an unrelated turn.
// Live prompt-injection defenses miss it, because it arrives as a stored fact. The
// defense is structural, because model-based poison detectors miss most of it.
//
//   - Trust is a pure function of provenance ([TrustOf]). Tool output, inbound
//     messages and fetched pages are untrusted, the operator is trusted, and the
//     agent's own run is between. A recalled memory carries its source's trust.
//   - Taint follows a run. An agent that reads poisoned output and writes a
//     conclusion as its own launders it, so a run carries a taint scope
//     ([NewTaintScope], [Observe]), the write records it ([TaintItem]), and the wake
//     digest, which reaches every reader unasked, admits only what
//     [PushEligibility] clears. A reader who asks can still recall a tainted fact.
//   - Ingest is screened ([Screen]) for invisible, bidi and tag-character smuggling
//     and for injection phrasing. A hit from an untrusted source is refused.
//
// The phrase screen is best effort; obfuscation, encoding, translation and splitting
// bypass it. The character screen is deterministic, and provenance trust means an
// undetected untrusted payload is still never acted on as trusted intent.
package guard

import (
	"sort"
	"strings"
	"unicode"

	"github.com/ionalpha/flynn/sandbox"
)

// Provenance scheme prefixes on a memory's Source, so trust needs no separate field.
// A source with no scheme (a run id, "chat") is the agent's own run, TrustSemi.
const (
	// SchemeUser marks content the operator wrote. Trusted.
	SchemeUser = "user:"
	// SchemeAgent marks content the agent authored for itself (a distilled lesson,
	// a converged run's note). Semi-trusted.
	SchemeAgent = "agent:"
	// SchemeTool marks tool output, which an attacker can influence. Untrusted.
	SchemeTool = "tool:"
	// SchemeInbound marks content from an inbound channel (a received message, a
	// webhook). Untrusted.
	SchemeInbound = "inbound:"
	// SchemeWeb marks content fetched from the network. Untrusted.
	SchemeWeb = "web:"
	// SchemeExternal marks content from any other outside origin. Untrusted.
	SchemeExternal = "external:"
)

// TrustOfAll returns the weakest trust among sources, because the untrusted input
// is in the content either way and the strongest would let one trusted source
// launder the rest. No sources is TrustSemi, as for an unlabelled source.
func TrustOfAll(sources []string) sandbox.Trust {
	if len(sources) == 0 {
		// Without this the loop's seed would report TrustTrusted.
		return TrustOf("")
	}
	worst := sandbox.TrustTrusted
	for _, s := range sources {
		// sandbox.Trust ascends toward TrustUntrusted, so weakest is largest.
		if t := TrustOf(s); t > worst {
			worst = t
		}
	}
	return worst
}

// TrustOf derives one source's trust. It is pure, so retrieval and the write gate
// agree. An unknown scheme is TrustSemi and never trusted, so a mistagged source is
// at worst under-trusted. See [TrustOfAll] for several sources.
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
	// KindInvisible is an invisible character such as a zero-width space, soft
	// hyphen or byte-order mark.
	KindInvisible FindingKind = "invisible-character"
	// KindBidi is a bidirectional control, which makes displayed text differ from
	// stored text.
	KindBidi FindingKind = "bidi-override"
	// KindTagChars is a Unicode tag-block payload (U+E0000 to U+E007F), which
	// encodes ASCII invisibly.
	KindTagChars FindingKind = "tag-character-payload"
	// KindInjectionPhrase is overt injection phrasing. It catches the obvious and
	// is easy to bypass.
	KindInjectionPhrase FindingKind = "injection-phrase"
)

// Finding is one screening hit.
type Finding struct {
	Kind   FindingKind
	Detail string
}

// Structural reports a deterministic character finding, as opposed to a phrase hit.
// The write gate refuses untrusted content on any finding; only structural ones are
// reliable.
func (f Finding) Structural() bool { return f.Kind != KindInjectionPhrase }

// injectionPhrases are lower-case and matched case-insensitively. The list is short
// because a longer one flags notes about prompt injection and still misses an
// adversary who obfuscates.
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

// Code points for the screens, written as escapes so the source holds no invisible
// glyph.
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

// isInvisible reports a character a reader would not see. Spaces, tabs and newlines
// are excluded. Bidi and tag characters are excluded too, because they have their
// own kinds.
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

// Screen returns every finding, structural ones first. An empty result guarantees
// nothing beyond these checks.
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
	// Sort by detail within each group for determinism.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Structural() != out[j].Structural() {
			return out[i].Structural()
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}
