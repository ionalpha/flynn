package skillrecall_test

import (
	"context"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/skill/skillrecall"
	"github.com/ionalpha/flynn/state"
)

// A term is a word, not a run of letters: "ion" is not in "integration", and "test"
// is not in "latest". An end that is punctuation has nothing to be a boundary of, so a
// path or a file name still matches where it is written.
func TestMatchScoreMatchesWholeWords(t *testing.T) {
	tests := []struct {
		name string
		term string
		text string
		want int
	}{
		{"inside a longer word", "ion", "external-integration", 0},
		{"inside a longer word, later", "ion", "version", 0},
		{"inside a longer word, middle", "ion", "translating", 0},
		{"suffix of a word", "test", "the latest build", 0},
		{"prefix of a word", "deploy", "deployed twice", 0},
		{"the word itself", "ion", "the ion alpha voice", 1},
		{"at the start", "ion", "ion alpha", 1},
		{"at the end", "alpha", "the ion alpha", 1},
		{"whole text", "ion", "ion", 1},
		{"beside a hyphen", "release", "pack-deploy-is-not-release", 1},
		{"beside a hyphen, left", "deploy", "pack-deploy-is-not-release", 1},
		{"after a whole-word miss", "ion", "integration, then ion", 1},
		{"after a whole-word miss, none", "ion", "integration and version", 0},
		{"path with a trailing slash", "core/", "edit core/animation today", 1},
		{"path with a trailing slash inside a word", "core/", "hardcore/animation", 0},
		{"file name", "render.js", "uses render.js here", 1},
		{"file name inside a longer name", "render.js", "uses prerender.js here", 0},
		{"file name before a longer name", "render.js", "uses render.json here", 0},
		{"identifier with a dot", "g.if", "call g.if(x)", 1},
		{"digits are word characters", "2xx", "a 2xx check", 1},
		{"digits inside a longer token", "2xx", "a 12xx check", 0},
		{"non-ASCII neighbour is a letter", "caf", "un café", 0},
		{"empty term", "", "anything", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skillrecall.MatchScore([]string{tt.term}, tt.text); got != tt.want {
				t.Errorf("MatchScore(%q, %q) = %d, want %d", tt.term, tt.text, got, tt.want)
			}
		})
	}
}

// Whatever the letters, a term never matches inside a longer word, and always matches
// as a word between any two non-word characters. Together they say the boundary is
// the whole rule.
func TestProp_ATermMatchesAsAWordAndNeverInsideOne(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		word := rapid.StringMatching(`[a-z0-9]{3,8}`).Draw(rt, "word")
		pre := rapid.StringMatching(`[a-z0-9]{0,3}`).Draw(rt, "pre")
		post := rapid.StringMatching(`[a-z0-9]{0,3}`).Draw(rt, "post")
		sep := rapid.SampledFrom([]string{" ", "-", "/", ".", ",", "(", ":"}).Draw(rt, "sep")
		terms := []string{word}

		if pre != "" || post != "" {
			if got := skillrecall.MatchScore(terms, pre+word+post); got != 0 {
				rt.Fatalf("%q matched inside %q", word, pre+word+post)
			}
		}
		if got := skillrecall.MatchScore(terms, "x"+sep+word+sep+"y"); got != 1 {
			rt.Fatalf("%q did not match as a word in %q", word, "x"+sep+word+sep+"y")
		}
	})
}

// The objective from the field report: "ion" is a word of the Ion Alpha voice and a
// fragment of "integration", "version" and "translating". The skill about
// integration matched three terms it has nothing to do with and was offered ahead of
// the writing skills.
func TestRecallDoesNotOfferASkillForAFragmentOfItsWords(t *testing.T) {
	store := library(t,
		state.Skill{
			Slug: "external-integration", Name: "external-integration",
			Description: "Integrating with a system you do not run: read the current version of its documentation before translating its examples into calls.",
			Tags:        []string{"integration"},
		},
		state.Skill{
			Slug: "release-notes", Name: "release-notes",
			Description: "Write release notes a reader can act on, in the Ion Alpha voice.",
		},
		state.Skill{
			Slug: "editorial-standard", Name: "editorial-standard",
			Description: "The Ion Alpha voice for anything a person will read: how to write it, and what to cut.",
		},
	)
	got := slugs(skillrecall.Recall(context.Background(), store, "write the release notes in the Ion Alpha voice", 0))
	if len(got) < 3 || got[0] != "release-notes" || got[2] != "external-integration" {
		t.Errorf("offered %v, want release-notes first and external-integration last: it shares only fragments of words with the objective", got)
	}
}

// The other field report: "not" and "yet" are two of the terms of an objective about
// a feature that is deployed and not released, and they carried a skill about
// unfinished interfaces ahead of the one whose subject it is.
func TestRecallRanksTheSubjectAboveTheFunctionWords(t *testing.T) {
	store := library(t,
		state.Skill{
			Slug: "ui-quality", Name: "ui-quality",
			Description: "Use when an interface is not finished yet: what it should do when it is empty, slow or wrong.",
		},
		state.Skill{
			Slug: "pack-deploy-is-not-release", Name: "pack-deploy-is-not-release",
			Description: "Use when something is deployed but the feature is not released: a deploy puts code on a host and a release exposes it to users.",
		},
		state.Skill{
			Slug: "code-health", Name: "code-health",
			Description: "What a duplicated function means and what it does not, and which debt markers to act on.",
		},
	)
	got := slugs(skillrecall.Recall(context.Background(), store, "we deployed but the feature is not released yet", 0))
	if len(got) == 0 || got[0] != "pack-deploy-is-not-release" {
		t.Errorf("offered %v, want pack-deploy-is-not-release first", got)
	}
}

// Keywords stop reading by position. A long input opens with whatever the writer
// opened with; the words it repeats are what it is about.
func TestKeywordsPutTheRepeatedSubjectFirst(t *testing.T) {
	in := "Thanks for the handoff, hope the week went well. Anyway: the sharding migration " +
		"is failing, and I think the migration locks the table when sharding starts."
	got := skillrecall.Keywords(in)
	if len(got) < 2 || got[0]+got[1] != "shardingmigration" && got[0]+got[1] != "migrationsharding" {
		t.Errorf("Keywords = %v, want sharding and migration first: the text repeats them", got)
	}
	if len(got) > 8 {
		t.Errorf("Keywords returned %d terms, want at most 8", len(got))
	}
}

// Recall searches wider than the eight terms it ranks on, so the subject of a long
// input is found when more than eight content words come before it.
func TestRecallFindsTheSubjectOfALongInput(t *testing.T) {
	store := library(t,
		state.Skill{Slug: "sharding-migration", Name: "sharding-migration", Description: "Move rows between shards without locking the table during a migration."},
		state.Skill{Slug: "lunch-planner", Name: "lunch-planner", Description: "Plan lunch for a team, with travel time and weather in mind."},
	)
	in := "Thanks for the handoff, hope the week went well and everything stayed calm over the holidays. " +
		"Several unrelated chores came up first, then the actual problem: the sharding migration fails."
	if first := skillrecall.Keywords(in)[0]; first == "sharding" {
		t.Fatalf("the input must put its subject past the eight-word cut for this test to mean anything")
	}
	got := slugs(skillrecall.Recall(context.Background(), store, in, 0))
	if len(got) == 0 || got[0] != "sharding-migration" {
		t.Errorf("offered %v, want sharding-migration first", got)
	}
}

// Rank is callable from a literal and keeps the eight terms that separate the
// candidates, so extra terms no candidate carries change nothing.
func TestRankIgnoresTermsNoCandidateCarries(t *testing.T) {
	cands := []skillrecall.Candidate{
		{Slug: "a", Offer: "Deploy the service."},
		{Slug: "b", Offer: "Deploy the service and its database."},
	}
	base := skillrecall.Rank([]string{"deploy", "database"}, cands, 0)
	padded := strings.Fields("zeta eta theta iota kappa lambda mu nu xi omicron")
	padded = append(padded, "deploy", "database")
	got := skillrecall.Rank(padded, cands, 0)
	if strings.Join(candidateSlugs(got), ",") != strings.Join(candidateSlugs(base), ",") {
		t.Errorf("ranked %v with unmatched terms ahead, want %v: a term nothing carries weighs nothing", candidateSlugs(got), candidateSlugs(base))
	}
}
