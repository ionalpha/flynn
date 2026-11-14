package recallplan_test

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/recallplan"
)

func TestPlan(t *testing.T) {
	for _, c := range []struct {
		name  string
		query string
		want  []recallplan.Attempt
	}{
		{"empty", "", nil},
		{"blank", "  \t ", nil},
		{"one word", "database", []recallplan.Attempt{{Terms: []string{"database"}}}},
		{"one word with punctuation and case", " Database? ", []recallplan.Attempt{{Terms: []string{"database"}}}},
		{"only punctuation is matched literally", "???", []recallplan.Attempt{{Terms: []string{"???"}}}},
		{"two content words", "database backups", []recallplan.Attempt{
			{Terms: []string{"database", "backups"}},
			{Terms: []string{"database", "backups"}, Any: true},
		}},
		{"a question", "What did we decide about the database?", []recallplan.Attempt{
			{Terms: []string{"what", "did", "we", "decide", "about", "the", "database"}},
			{Terms: []string{"decide", "database"}},
			{Terms: []string{"decide", "database"}, Any: true},
		}},
		{"one content word among stop words", "where is the database", []recallplan.Attempt{
			{Terms: []string{"where", "is", "the", "database"}},
			{Terms: []string{"database"}},
		}},
		{"only stop words are kept, not dropped", "what is the", []recallplan.Attempt{
			{Terms: []string{"what", "is", "the"}},
			{Terms: []string{"what", "is", "the"}, Any: true},
		}},
		{"repeats collapse", "db DB db", []recallplan.Attempt{{Terms: []string{"db"}}}},
		{"query syntax stays text", `database AND "x"`, []recallplan.Attempt{
			{Terms: []string{"database", "and", "x"}},
			{Terms: []string{"database", "x"}},
			{Terms: []string{"database", "x"}, Any: true},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := recallplan.Plan(c.query)
			if !slices.EqualFunc(got, c.want, func(a, b recallplan.Attempt) bool {
				return a.Any == b.Any && slices.Equal(a.Terms, b.Terms)
			}) {
				t.Fatalf("Plan(%q) = %+v, want %+v", c.query, got, c.want)
			}
		})
	}
}

// TestPlanOrdering states the policy as properties over any query: the plan is
// strict first, a widened attempt is last and is the only one that accepts a
// partial match, and no attempt is empty or repeats another.
func TestPlanOrdering(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		query := rapid.String().Draw(t, "query")
		plan := recallplan.Plan(query)
		for i, a := range plan {
			if len(a.Terms) == 0 {
				t.Fatalf("attempt %d of %q has no terms", i, query)
			}
			if a.Any && i != len(plan)-1 {
				t.Fatalf("attempt %d of %q accepts any term but is not last", i, query)
			}
			if a.Any && len(a.Terms) < 2 {
				t.Fatalf("attempt %d of %q accepts any of fewer than two terms", i, query)
			}
			for _, b := range plan[:i] {
				if a.Any == b.Any && slices.Equal(a.Terms, b.Terms) {
					t.Fatalf("plan of %q repeats %+v", query, a)
				}
			}
		}
		if strings.TrimSpace(query) != "" && len(plan) == 0 {
			t.Fatalf("a non-empty query %q has no attempt", query)
		}
	})
}

func TestSelect(t *testing.T) {
	text := func(s string) string { return s }
	items := []string{
		"we decided to keep the database on postgres after the review",
		"the review of the pipeline",
		"database backups run nightly",
	}
	hits := func(query string, floor float64) []string {
		var out []string
		for _, h := range recallplan.Select(query, items, text, floor) {
			out = append(out, h.Item)
		}
		return out
	}

	t.Run("all words, any position", func(t *testing.T) {
		if got := hits("review keep database", 0); !slices.Equal(got, items[:1]) {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("a stricter attempt that answers stops the search", func(t *testing.T) {
		if got := hits("which database did we keep after the review", 0); !slices.Equal(got, items[:1]) {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("widens when nothing answers in full, with the share it holds", func(t *testing.T) {
		got := recallplan.Select("database nightly restore", items, text, 0)
		if len(got) != 2 {
			t.Fatalf("got %+v, want two partial hits", got)
		}
		for _, h := range got {
			want := map[string]float64{items[0]: 1.0 / 3, items[2]: 2.0 / 3}[h.Item]
			if diff := h.Share - want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("%q has share %v, want %v", h.Item, h.Share, want)
			}
		}
	})
	t.Run("the floor is applied before deciding to widen", func(t *testing.T) {
		// The strict attempt matches nothing and the widened one has hits at 1/3
		// and 2/3; a floor of 0.5 leaves only the better of them.
		if got := hits("database nightly restore", 0.5); !slices.Equal(got, items[2:]) {
			t.Fatalf("got %q", got)
		}
		if got := hits("database nightly restore", 0.9); got != nil {
			t.Fatalf("got %q, want none above the floor", got)
		}
	})
	t.Run("nothing matches", func(t *testing.T) {
		if got := hits("kubernetes terraform", 0); got != nil {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("an empty query selects nothing", func(t *testing.T) {
		if got := hits("", 0); got != nil {
			t.Fatalf("got %q", got)
		}
	})
}
