package bus_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/bus"
)

// FuzzMatch throws arbitrary pattern/subject pairs at the subject grammar:
// strings with empty tokens, wildcards in odd places, NULs, whitespace, and
// non-ASCII. It asserts the invariants hold on any input: Match and the
// validators never panic, and a concrete subject always matches itself.
func FuzzMatch(f *testing.F) {
	seeds := []struct{ pattern, subject string }{
		{"a.b.c", "a.b.c"},
		{"a.*", "a.b"},
		{"a.>", "a.b.c"},
		{">", "x.y"},
		{"a.>.c", "a.b.c"},
		{"", ""},
		{"a..b", "a.b"},
		{"*.🔥", "a.🔥"},
		{"a\tb", "a\tb"},
		{"a.b\x00", "a.b\x00"},
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.subject)
	}

	f.Fuzz(func(t *testing.T, pattern, subject string) {
		// None of these may panic on any input.
		_ = bus.Match(pattern, subject)
		validSubj := bus.ValidSubject(subject)
		_ = bus.ValidPattern(pattern)

		// A valid concrete subject must match itself, and must also be a legal
		// pattern (a published subject can always be subscribed to verbatim).
		if validSubj {
			if !bus.Match(subject, subject) {
				t.Fatalf("valid subject %q does not match itself", subject)
			}
			if !bus.ValidPattern(subject) {
				t.Fatalf("valid subject %q is not a valid pattern", subject)
			}
		}

		// Match must never accept a subject with an empty token via a literal
		// (non-wildcard) pattern token, because a real subject never has one.
		if bus.Match(pattern, subject) && strings.Contains(subject, "..") {
			// Subjects with empty tokens are not valid; if such a string matches,
			// it can only be through wildcards, never as an addressable subject.
			if validSubj {
				t.Fatalf("subject %q with empty token reported valid", subject)
			}
		}
	})
}
