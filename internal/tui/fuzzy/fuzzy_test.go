package fuzzy_test

import (
	"strings"
	"testing"
	"unicode"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/fuzzy"
)

func TestEmptyPatternMatchesEverythingAtZero(t *testing.T) {
	for _, c := range []string{"", "a", "screen/render.go"} {
		s, ok := fuzzy.Score("", c)
		if !ok || s != 0 {
			t.Fatalf("Score(%q, %q) = %d, %v; want 0, true", "", c, s, ok)
		}
	}
}

func TestNonSubsequenceDoesNotMatch(t *testing.T) {
	if _, ok := fuzzy.Score("xyz", "screen/render.go"); ok {
		t.Fatal("xyz matched screen/render.go")
	}
	if _, ok := fuzzy.Score("ab", "ba"); ok {
		t.Fatal("out-of-order letters matched")
	}
}

func TestMatchingIsCaseInsensitiveWithExactCaseBonus(t *testing.T) {
	folded, ok := fuzzy.Score("readme", "README.md")
	if !ok {
		t.Fatal("case-folded pattern did not match")
	}
	exact, ok := fuzzy.Score("README", "README.md")
	if !ok {
		t.Fatal("exact-case pattern did not match")
	}
	if exact <= folded {
		t.Fatalf("exact-case score %d not above folded %d", exact, folded)
	}
}

func TestSegmentStartsOutrankScatteredLetters(t *testing.T) {
	segment, _ := fuzzy.Score("sre", "screen/render.go")
	scatter, ok := fuzzy.Score("sre", "assured")
	if !ok {
		t.Fatal("scatter candidate did not match")
	}
	if segment <= scatter {
		t.Fatalf("segment-start score %d not above scatter %d", segment, scatter)
	}
}

func TestConsecutiveRunOutranksGaps(t *testing.T) {
	run, _ := fuzzy.Score("rend", "render.go")
	gaps, ok := fuzzy.Score("rend", "read-end.go")
	if !ok {
		t.Fatal("gapped candidate did not match")
	}
	if run <= gaps {
		t.Fatalf("consecutive score %d not above gapped %d", run, gaps)
	}
}

func TestCamelCaseHumpEarnsABonus(t *testing.T) {
	hump, _ := fuzzy.Score("fb", "fooBar")
	flat, ok := fuzzy.Score("fb", "foombr")
	if !ok {
		t.Fatal("flat candidate did not match")
	}
	if hump <= flat {
		t.Fatalf("camel score %d not above flat %d", hump, flat)
	}
}

func TestRankOrdersBestFirstAndHonorsLimit(t *testing.T) {
	items := []string{"assured", "screen/render.go", "shared/red.go", "unrelated.txt"}
	got := fuzzy.Rank("sre", items, 2, nil)
	if len(got) != 2 {
		t.Fatalf("Rank returned %d items, want 2", len(got))
	}
	if got[0] != "screen/render.go" && got[0] != "shared/red.go" {
		t.Fatalf("best match %q is not a segment-start candidate", got[0])
	}
}

func TestRankDropsNonMatches(t *testing.T) {
	got := fuzzy.Rank("zzz", []string{"a", "b"}, 10, nil)
	if len(got) != 0 {
		t.Fatalf("Rank kept non-matches: %v", got)
	}
}

func TestRankTiesBreakByLengthThenLexically(t *testing.T) {
	got := fuzzy.Rank("", []string{"bb", "a", "ab"}, 10, nil)
	want := []string{"a", "ab", "bb"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tie-break order %v, want %v", got, want)
		}
	}
}

func TestRankBonusPromotesACandidate(t *testing.T) {
	items := []string{"aaa/pick.go", "bbb/pick.go"}
	got := fuzzy.Rank("pick", items, 1, func(s string) int {
		if s == "bbb/pick.go" {
			return 100
		}
		return 0
	})
	if len(got) != 1 || got[0] != "bbb/pick.go" {
		t.Fatalf("bonus did not promote: %v", got)
	}
}

func TestRankZeroLimitReturnsNothing(t *testing.T) {
	if got := fuzzy.Rank("a", []string{"a"}, 0, nil); got != nil {
		t.Fatalf("limit 0 returned %v", got)
	}
}

// TestScoreProperties: a reported match is always a case-insensitive
// subsequence, and scoring is deterministic.
func TestScoreProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pattern := rapid.StringN(0, 8, 64).Draw(t, "pattern")
		candidate := rapid.StringN(0, 32, 256).Draw(t, "candidate")
		s1, ok1 := fuzzy.Score(pattern, candidate)
		s2, ok2 := fuzzy.Score(pattern, candidate)
		if s1 != s2 || ok1 != ok2 {
			t.Fatalf("Score not deterministic: (%d,%v) then (%d,%v)", s1, ok1, s2, ok2)
		}
		if ok1 != isSubsequenceFold(pattern, candidate) {
			t.Fatalf("Score(%q, %q) ok=%v disagrees with subsequence check", pattern, candidate, ok1)
		}
	})
}

// TestRankProperties: every ranked item matches, the limit holds, and the
// output is deterministic.
func TestRankProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pattern := rapid.StringN(0, 4, 32).Draw(t, "pattern")
		items := rapid.SliceOfN(rapid.StringN(0, 16, 128), 0, 20).Draw(t, "items")
		limit := rapid.IntRange(0, 25).Draw(t, "limit")
		got := fuzzy.Rank(pattern, items, limit, nil)
		if len(got) > limit {
			t.Fatalf("Rank returned %d items above limit %d", len(got), limit)
		}
		for _, g := range got {
			if _, ok := fuzzy.Score(pattern, g); !ok {
				t.Fatalf("ranked item %q does not match %q", g, pattern)
			}
		}
		again := fuzzy.Rank(pattern, items, limit, nil)
		if strings.Join(got, "\x00") != strings.Join(again, "\x00") {
			t.Fatalf("Rank not deterministic: %v then %v", got, again)
		}
	})
}

// isSubsequenceFold is the independent oracle for the match predicate.
func isSubsequenceFold(pattern, s string) bool {
	ps := []rune(pattern)
	i := 0
	for _, r := range s {
		if i < len(ps) && unicode.ToLower(r) == unicode.ToLower(ps[i]) {
			i++
		}
	}
	return i == len(ps)
}
