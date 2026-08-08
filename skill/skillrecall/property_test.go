package skillrecall_test

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill/skillrecall"
	"github.com/ionalpha/flynn/state"
)

// Every term Keywords produces is a lowercased content word of the objective, and
// there are never more than the cap. The terms become one store query each, so a
// term that is not in the objective searches for something nobody asked about, and
// an uncapped list turns a long objective into a long scan.
func TestProp_KeywordsAreBoundedContentWordsOfTheObjective(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		objective := rapid.String().Draw(rt, "objective")
		terms := skillrecall.Keywords(objective)
		if len(terms) > 8 {
			rt.Fatalf("%d terms from %q, want at most 8", len(terms), objective)
		}
		lower := strings.ToLower(objective)
		seen := map[string]bool{}
		for _, term := range terms {
			if seen[term] {
				rt.Fatalf("term %q repeated: it would search the same query twice", term)
			}
			seen[term] = true
			if len(term) < 3 {
				rt.Fatalf("term %q is shorter than the floor", term)
			}
			if term != strings.ToLower(term) {
				rt.Fatalf("term %q is not lowercased", term)
			}
			if !strings.Contains(lower, term) {
				rt.Fatalf("term %q is not in %q", term, objective)
			}
		}
	})
}

// Rank is a selection: it returns a prefix-sized subset of what it was given, never
// invents a skill, never repeats one, and orders the same input the same way twice.
// The last of those is what makes the pack's retrieval table a check rather than a
// coin toss, since a table that passed on one run and failed on the next would be
// switched off within a week.
func TestProp_RankSelectsDeterministicallyFromItsCandidates(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 12).Draw(rt, "candidates")
		cands := make([]state.Skill, n)
		for i := range cands {
			cands[i] = state.Skill{
				ID:          fmt.Sprintf("id-%d", i),
				Slug:        fmt.Sprintf("skill-%d", i),
				Name:        rapid.StringMatching(`[a-z ]{0,20}`).Draw(rt, "name"),
				Description: rapid.StringMatching(`[a-z ]{0,40}`).Draw(rt, "description"),
				Reads:       rapid.IntRange(0, 30).Draw(rt, "reads"),
			}
			cands[i].Wins = rapid.IntRange(0, cands[i].Reads).Draw(rt, "wins")
			if rapid.Bool().Draw(rt, "verified") {
				cands[i].Tags = []string{"verified"}
			}
		}
		limit := rapid.IntRange(-2, 8).Draw(rt, "limit")
		terms := skillrecall.Keywords(rapid.StringMatching(`[a-z ]{0,40}`).Draw(rt, "objective"))

		got := skillrecall.Rank(terms, cands, limit)
		want := limit
		if want <= 0 {
			want = skillrecall.DefaultLimit
		}
		if want > n {
			want = n
		}
		if len(got) != want {
			rt.Fatalf("ranked %d of %d candidates with limit %d, want %d", len(got), n, limit, want)
		}
		known := map[string]bool{}
		for _, c := range cands {
			known[c.ID] = true
		}
		seen := map[string]bool{}
		for _, s := range got {
			if !known[s.ID] {
				rt.Fatalf("ranked %s, which was not a candidate", s.ID)
			}
			if seen[s.ID] {
				rt.Fatalf("ranked %s twice", s.ID)
			}
			seen[s.ID] = true
		}
		again := skillrecall.Rank(terms, cands, limit)
		for i := range got {
			if got[i].ID != again[i].ID {
				rt.Fatalf("the same input ranked differently twice at position %d", i)
			}
		}
	})
}

// A row written by the parser's own rules comes back as it was written. The table is
// hand-edited alongside a skill, so a column that silently loses a slug, or an
// objective that comes back trimmed differently, makes an assertion nobody wrote.
func TestProp_TableRowsSurviveParsing(t *testing.T) {
	slug := rapid.StringMatching(`[a-z][a-z-]{0,12}`)
	rapid.Check(t, func(rt *rapid.T) {
		objective := strings.TrimSpace(rapid.StringMatching(`[a-zA-Z0-9 ]{1,40}`).Draw(rt, "objective"))
		if objective == "" {
			return
		}
		offered := rapid.SliceOfNDistinct(slug, 0, 3, func(s string) string { return s }).Draw(rt, "offered")
		absent := rapid.SliceOfNDistinct(slug, 0, 3, func(s string) string { return s }).Draw(rt, "absent")
		if len(offered)+len(absent) == 0 {
			return
		}
		row := fmt.Sprintf("%s | %s | %s", objective, strings.Join(offered, ", "), strings.Join(absent, ","))

		table, err := skillrecall.ParseTable([]byte(row))
		if err != nil {
			rt.Fatalf("parse %q: %v", row, err)
		}
		if len(table.Cases) != 1 {
			rt.Fatalf("parsed %d rows from one line", len(table.Cases))
		}
		c := table.Cases[0]
		if c.Objective != objective {
			rt.Fatalf("objective came back %q, want %q", c.Objective, objective)
		}
		if strings.Join(c.Offered, ",") != strings.Join(offered, ",") {
			rt.Fatalf("offered came back %v, want %v", c.Offered, offered)
		}
		if strings.Join(c.Absent, ",") != strings.Join(absent, ",") {
			rt.Fatalf("absent came back %v, want %v", c.Absent, absent)
		}
	})
}
