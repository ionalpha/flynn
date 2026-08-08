package curate_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/memory/curate"
	"github.com/ionalpha/flynn/state"
)

// The measure is judged by the two answers it has to get right at once: a fork
// scores above the default threshold, and two topics that merely share a word
// score below it. Getting only the first right is a measure that reports every
// subject ending in "-choice" as a fork of every other.
func TestSubjectSimilarity(t *testing.T) {
	const threshold = 0.6
	for _, tc := range []struct {
		name   string
		a, b   string
		isFork bool
	}{
		{"identical", "db-choice", "db-choice", true},
		{"a contraction", "database-choice", "db-choice", true},
		{"a truncation", "prod-deployment", "prod-deploy", true},
		{"a plural", "api-key", "api-keys", true},
		{"a single token contracted", "database", "db", true},
		{"an extra qualifier", "db-choice", "db-choice-2026", true},
		{"one shared word, two topics", "db-choice", "queue-choice", false},
		{"nothing in common", "db-choice", "release-cadence", false},
		{"a shared first letter is not an abbreviation", "d-choice", "database-choice", false},
		{"a coincidental subsequence", "cache-policy", "chance-policy", false},
		{"synonyms it cannot see", "db-choice", "storage-engine", false},
		{"empty against anything", "", "db-choice", false},
		{"both empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := curate.SubjectSimilarity(tc.a, tc.b)
			if (got >= threshold) != tc.isFork {
				t.Fatalf("SubjectSimilarity(%q, %q) = %.3f, want %s the %.1f threshold",
					tc.a, tc.b, got, map[bool]string{true: "at or above", false: "below"}[tc.isFork], threshold)
			}
		})
	}
}

// Symmetry is a contract, not a coincidence: the pair arrives in whatever order
// the store listed it, so a measure that scored one direction higher would report
// a fork or not depending on which subject happened to be written first.
func TestProp_SubjectSimilarityIsSymmetricAndBounded(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := drawSubject(rt, "a")
		b := drawSubject(rt, "b")
		ab, ba := curate.SubjectSimilarity(a, b), curate.SubjectSimilarity(b, a)
		if ab != ba {
			rt.Fatalf("SubjectSimilarity(%q, %q) = %.3f but the reverse = %.3f", a, b, ab, ba)
		}
		if ab < 0 || ab > 1 {
			rt.Fatalf("SubjectSimilarity(%q, %q) = %.3f, outside [0,1]", a, b, ab)
		}
		if a != "" && a == b && ab != 1 {
			rt.Fatalf("SubjectSimilarity(%q, %q) = %.3f, want 1 for identical subjects", a, b, ab)
		}
	})
}

// drawSubject generates a normalized subject: the only input the measure is ever
// handed, since every write path normalizes before anything keys on it.
func drawSubject(rt *rapid.T, label string) string {
	raw := rapid.StringMatching(`[a-z0-9 _-]{0,20}`).Draw(rt, label)
	out, err := state.NormalizeSubject(raw)
	if err != nil {
		return ""
	}
	return out
}
