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
// is not in "latest". An inflected form of the word still counts ("paste" and
// "pasting", "run" and "running"). An end that is punctuation has nothing to be a
// boundary of, so a path or a file name still matches where it is written.
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
		{"prefix of a word", "deploy", "a deployment plan", 0},
		{"prefix of a longer word", "pack", "packaging", 0},
		{"the word itself", "ion", "the ion channel", 1},
		{"at the start", "ion", "ion channel", 1},
		{"at the end", "channel", "the ion channel", 1},
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
		{"plural of the term", "rule", "the rules apply", 1},
		{"term is the plural", "rules", "one rule applies", 1},
		{"es plural", "box", "three boxes", 1},
		{"es plural, other way", "boxes", "a box", 1},
		{"past tense of the term", "deploy", "deployed twice", 1},
		{"term is the past tense", "renamed", "rename this function", 1},
		{"past tense after a dropped e", "rename", "it was renamed", 1},
		{"participle of the term", "paste", "pasting a snippet", 1},
		{"term is the participle", "pasting", "can I paste this snippet", 1},
		{"doubled consonant", "run", "running the suite", 1},
		{"doubled consonant, other way", "running", "run the suite", 1},
		{"doubled consonant in a past tense", "stop", "stopped early", 1},
		{"plural of a participle", "setting", "the settings page", 1},
		{"sibilant plural", "cache", "clear the caches", 1},
		{"inflection beside a hyphen", "paste", "no-pasting-here", 1},
		{"short ing word is not folded", "bring", "the br tag", 0},
		{"ing word with no vowel stem", "string", "the str value", 0},
		{"inflection does not reach into a longer word", "ion", "integrations", 0},
		{"inflection does not reach into a longer word, ing", "test", "testament", 0},
		{"different words stay apart", "paste", "pasta salad", 0},
		{"a path is matched as written", "core/", "edit cores/animation", 0},
		{"inflection of a different word", "rename", "renamer", 0},
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
// the whole rule. A longer word here is one that is not an inflection of the term: a
// prefix, a digit run, or a derived suffix such as "ment" or "ness".
func TestProp_ATermMatchesAsAWordAndNeverInsideOne(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		word := rapid.StringMatching(`[a-z0-9]{3,8}`).Draw(rt, "word")
		pre := rapid.StringMatching(`[a-z0-9]{1,3}`).Draw(rt, "pre")
		digits := rapid.StringMatching(`[0-9]{1,3}`).Draw(rt, "digits")
		suffix := rapid.SampledFrom([]string{"ment", "ness", "tion", "er", "ly", "able", "al"}).Draw(rt, "suffix")
		sep := rapid.SampledFrom([]string{" ", "-", "/", ".", ",", "(", ":"}).Draw(rt, "sep")
		terms := []string{word}

		for _, in := range []string{pre + word, word + digits, pre + word + digits, word + suffix, pre + word + suffix} {
			if got := skillrecall.MatchScore(terms, in); got != 0 {
				rt.Fatalf("%q matched inside %q", word, in)
			}
		}
		if got := skillrecall.MatchScore(terms, "x"+sep+word+sep+"y"); got != 1 {
			rt.Fatalf("%q did not match as a word in %q", word, "x"+sep+word+sep+"y")
		}
	})
}

// "ion" is a word of the objective and a fragment of "integration", "version" and
// "translating". The skill about integration matched three terms it has nothing to do
// with and was offered ahead of the writing skills.
func TestRecallDoesNotOfferASkillForAFragmentOfItsWords(t *testing.T) {
	store := library(t,
		state.Skill{
			Slug: "external-integration", Name: "external-integration",
			Description: "Integrating with a system you do not run: read the current version of its documentation before translating its examples into calls.",
			Tags:        []string{"integration"},
		},
		state.Skill{
			Slug: "release-notes", Name: "release-notes",
			Description: "Write release notes a reader can act on, in the ion channel style.",
		},
		state.Skill{
			Slug: "editorial-standard", Name: "editorial-standard",
			Description: "The ion channel style for anything a person will read: how to write it, and what to cut.",
		},
	)
	got := slugs(skillrecall.Recall(context.Background(), store, "write the release notes in the ion channel style", 0))
	if len(got) < 3 || got[0] != "release-notes" || got[2] != "external-integration" {
		t.Errorf("offered %v, want release-notes first and external-integration last: it shares only fragments of words with the objective", got)
	}
}

// "not" and "yet" are two of the terms of an objective about
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

// The store matches words as written, so a skill that says "pasting" is only found
// for the objective "paste" if the search also asks for the inflected forms.
func TestRecallOffersASkillThatUsesAnotherFormOfTheWord(t *testing.T) {
	tests := []struct {
		name      string
		objective string
		skill     string
	}{
		{"participle for the base", "can I paste this snippet", "Covers copying, porting or pasting code from outside."},
		{"past tense for the base", "rename this function", "A name that no longer describes the behaviour has to be renamed."},
		{"base for the participle", "we are pasting a snippet", "Use when you paste code from another project."},
		{"doubled consonant", "run the suite", "Covers running the suite in CI."},
		{"plural", "clear the cache", "Explains why the caches go stale."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := library(t,
				state.Skill{Slug: "target", Name: "target", Description: tt.skill},
				state.Skill{Slug: "other", Name: "other", Description: "Plan lunch for a team."},
			)
			got := slugs(skillrecall.Recall(context.Background(), store, tt.objective, 1))
			if len(got) != 1 || got[0] != "target" {
				t.Errorf("offered %v for %q, want target", got, tt.objective)
			}
		})
	}
}
