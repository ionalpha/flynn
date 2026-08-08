package skillstyle_test

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill/skillstyle"
)

// refused are the marks the gate exists to catch, one per rule that fires on a
// single inserted token.
var refused = []string{"—", "―", "–", "…"}

// Property: a refused mark is found wherever it is put, at the position it was put,
// and prose without one is left alone whatever it is made of.
//
// Stated as a property rather than as cases because the two ways this check fails in
// practice are both about position rather than about the mark: a rule anchored to the
// start of a line, and a column counted in bytes so a finding points into the middle
// of a multi-byte character. Both survive any example a person thinks to write and
// neither survives a generator that puts the mark after arbitrary text.
func TestProp_AMarkIsFoundWhereItWasPut(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Words with no refused mark in them, including multi-byte ones, so the column
		// is exercised against text that is longer in bytes than in runes.
		word := rapid.SampledFrom([]string{"read", "the", "diff", "commit", "rebase", "café", "naïve", "日本語"})
		before := strings.Join(rapid.SliceOfN(word, 0, 6).Draw(rt, "before"), " ")
		after := strings.Join(rapid.SliceOfN(word, 0, 6).Draw(rt, "after"), " ")

		if got := skillstyle.Check("SKILL.md", []byte(before+" "+after)); len(got) != 0 {
			rt.Fatalf("clean prose was refused:\n%s", skillstyle.Report(got))
		}

		mark := rapid.SampledFrom(refused).Draw(rt, "mark")
		line := before + mark + after
		got := skillstyle.Check("SKILL.md", []byte(line))
		if len(got) != 1 {
			rt.Fatalf("Check(%q) returned %d findings, want 1:\n%s", line, len(got), skillstyle.Report(got))
		}
		if got[0].Match != mark {
			rt.Errorf("matched %q, want %q", got[0].Match, mark)
		}
		if want := len([]rune(before)) + 1; got[0].Column != want {
			rt.Errorf("column %d, want %d (counted in runes, not bytes)", got[0].Column, want)
		}
		if got[0].Line != 1 {
			rt.Errorf("line %d, want 1", got[0].Line)
		}
	})
}

// Property: every mark in a document is reported, on the line it is on, whatever the
// shape of the document. A gate that reports the first one turns fixing a pack into
// one CI run per mark.
func TestProp_EveryMarkOnEveryLineIsReported(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		counts := rapid.SliceOfN(rapid.IntRange(0, 3), 1, 6).Draw(rt, "marksPerLine")
		lines := make([]string, len(counts))
		total := 0
		for i, n := range counts {
			lines[i] = "read the diff" + strings.Repeat("— and again ", n)
			total += n
		}
		doc := strings.Join(lines, "\n")

		got := skillstyle.Check("SKILL.md", []byte(doc))
		if len(got) != total {
			rt.Fatalf("got %d findings for %d marks:\n%s", len(got), total, skillstyle.Report(got))
		}
		for _, f := range got {
			if f.Line < 1 || f.Line > len(lines) {
				rt.Fatalf("finding on line %d, outside a %d-line document", f.Line, len(lines))
			}
			if counts[f.Line-1] == 0 {
				rt.Errorf("finding on line %d, which holds no mark", f.Line)
			}
		}
	})
}
