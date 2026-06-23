package bus_test

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/bus"
)

// genSubject draws a valid concrete subject: 1-4 non-empty lowercase tokens.
func genSubject(rt *rapid.T) string {
	toks := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,5}`), 1, 4).Draw(rt, "tokens")
	return strings.Join(toks, ".")
}

// Property: any valid subject matches itself (the exact-match identity).
func TestProp_SubjectMatchesItself(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := genSubject(rt)
		if !bus.ValidSubject(s) {
			rt.Fatalf("generated subject %q is not valid", s)
		}
		if !bus.Match(s, s) {
			rt.Fatalf("Match(%q, %q) = false, want true", s, s)
		}
	})
}

// Property: the root tail pattern ">" matches every valid subject.
func TestProp_RootTailMatchesEverything(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := genSubject(rt)
		if !bus.Match(">", s) {
			rt.Fatalf(`Match(">", %q) = false, want true`, s)
		}
	})
}

// Property: replacing exactly one token of a subject with "*" still matches that
// subject (single-token wildcard).
func TestProp_StarSubstitutionMatches(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		toks := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,5}`), 1, 4).Draw(rt, "tokens")
		subject := strings.Join(toks, ".")
		i := rapid.IntRange(0, len(toks)-1).Draw(rt, "pos")

		pat := append([]string(nil), toks...)
		pat[i] = bus.TokenAny
		pattern := strings.Join(pat, ".")

		if !bus.Match(pattern, subject) {
			rt.Fatalf("Match(%q, %q) = false, want true", pattern, subject)
		}
	})
}

// Property: a non-tail pattern with a different token count never matches (a
// pattern matches only subjects of the same arity unless it ends in ">").
func TestProp_ArityMismatchNeverMatches(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		subjToks := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,5}`), 1, 4).Draw(rt, "subj")
		subject := strings.Join(subjToks, ".")
		// A pattern with strictly more tokens, all "*", and no tail.
		extra := rapid.IntRange(1, 3).Draw(rt, "extra")
		pat := make([]string, len(subjToks)+extra)
		for i := range pat {
			pat[i] = bus.TokenAny
		}
		pattern := strings.Join(pat, ".")
		if bus.Match(pattern, subject) {
			rt.Fatalf("Match(%q, %q) = true, want false (arity mismatch)", pattern, subject)
		}
	})
}

// Property: ValidSubject implies ValidPattern (every concrete subject is also a
// legal subscription pattern), but not the reverse.
func TestProp_ValidSubjectIsValidPattern(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := genSubject(rt)
		if bus.ValidSubject(s) && !bus.ValidPattern(s) {
			rt.Fatalf("%q is a valid subject but not a valid pattern", s)
		}
	})
}
