package skillrecall_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/skill/skillrecall"
	"github.com/ionalpha/flynn/state"
)

func TestKeywords(t *testing.T) {
	got := skillrecall.Keywords("Run the deploy for THIS service, with a 2xx check")
	want := []string{"deploy", "service", "2xx", "check"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Keywords = %v, want %v (stopwords and short words dropped, lowercased, deduped)", got, want)
	}
	// Eight terms is the cap, so a long objective does not turn into a long scan.
	long := skillrecall.Keywords("alpha bravo charlie delta echo foxtrot golf hotel india juliet")
	if len(long) != 8 {
		t.Errorf("Keywords returned %d terms for a ten-word objective, want the cap of 8", len(long))
	}
	// An objective with nothing to search on yields nothing, which is what stops
	// recall short rather than searching for every skill in the library.
	if got := skillrecall.Keywords("do it for me"); len(got) != 0 {
		t.Errorf("Keywords(%q) = %v, want none", "do it for me", got)
	}
}

// An objective made entirely of stopwords is offered nothing. Recall is an
// enrichment: with no term to search on there is no relevance to rank, and offering
// the library's first five skills would spend the prompt on noise.
func TestRecallOffersNothingWithoutTerms(t *testing.T) {
	store := library(t, debugging(), migration())
	if got := skillrecall.Recall(context.Background(), store, "do it for me", 0); len(got) != 0 {
		t.Errorf("Recall on a stopword objective offered %d skills, want none", len(got))
	}
}

// The offer is capped, and the cap is what makes recall precision-first. The skills
// below all match, so which ones survive is the ranker's decision rather than the
// store's order.
func TestRecallCapsTheOffer(t *testing.T) {
	var seeds []state.Skill
	for _, slug := range []string{"a-deploy", "b-deploy", "c-deploy", "d-deploy", "e-deploy", "f-deploy"} {
		seeds = append(seeds, state.Skill{Slug: slug, Name: slug, Description: "How to deploy the service."})
	}
	store := library(t, seeds...)
	if got := skillrecall.Recall(context.Background(), store, "deploy the service", 0); len(got) != skillrecall.DefaultLimit {
		t.Errorf("Recall offered %d skills, want the default limit of %d", len(got), skillrecall.DefaultLimit)
	}
	if got := skillrecall.Recall(context.Background(), store, "deploy the service", 2); len(got) != 2 {
		t.Errorf("Recall with limit 2 offered %d skills", len(got))
	}
}

// Evidence breaks a tie and relevance does not lose to it. Two skills that say the
// same thing are separated by a passing check and a confirmed record; a skill that
// matches more of the objective still outranks a decorated one that matches less.
func TestRankBreaksTiesOnEvidenceAndNeverOverRelevance(t *testing.T) {
	same := "How to deploy the service."
	plain := state.Skill{Slug: "plain", Name: "plain", Description: same}
	verified := state.Skill{Slug: "verified-one", Name: "verified-one", Description: same, Tags: []string{"verified"}}
	proven := state.Skill{Slug: "proven", Name: "proven", Description: same, Reads: 20, Wins: 19}

	terms := skillrecall.Keywords("deploy the service")
	ranked := skillrecall.Rank(terms, []state.Skill{plain, proven, verified}, 0)
	if ranked[0].Slug != "verified-one" {
		t.Errorf("ranked %s first, want the verified skill: a passing check outranks a record", ranked[0].Slug)
	}
	if ranked[1].Slug != "proven" {
		t.Errorf("ranked %s second, want the proven one ahead of the plain one", ranked[1].Slug)
	}

	// Relevance dominates: an unverified skill that carries both terms beats a
	// verified one that carries one.
	narrow := state.Skill{Slug: "narrow", Name: "narrow", Description: "About deploys.", Tags: []string{"verified"}}
	broad := state.Skill{Slug: "broad", Name: "broad", Description: "How to deploy the service."}
	if got := skillrecall.Rank(terms, []state.Skill{narrow, broad}, 0); got[0].Slug != "broad" {
		t.Errorf("ranked %s first; verification must break ties, not beat relevance", got[0].Slug)
	}
}

// The best answer is offered however its name sorts. A store answers a term with
// its best matches capped at what it was asked for, so recall asking for exactly
// the offer limit used to hand ranking a set the store had already cut, and for a
// word much of a library shares that cut is alphabetical: the skill that is
// actually about the objective was never a candidate, and nothing reported it.
func TestRecallOffersTheBestMatchWhateverItsSlugSortsAs(t *testing.T) {
	var seeds []state.Skill
	for _, slug := range []string{"a-one", "b-two", "c-three", "d-four", "e-five", "f-six", "g-seven"} {
		seeds = append(seeds, state.Skill{
			Slug: slug, Name: slug,
			Description: "Something about the database.",
			// Mentions the word in passing, so it matches the term and is a worse
			// answer than the skill whose subject it is.
			Body: "A migration is mentioned here once.",
		})
	}
	best := state.Skill{
		Slug: "z-migrations", Name: "z-migrations",
		Description: "How to run a database migration safely.",
	}
	store := library(t, append(seeds, best)...)

	got := skillrecall.Recall(context.Background(), store, "run a database migration", 0)
	if len(got) == 0 || got[0].Slug != "z-migrations" {
		t.Errorf("Recall offered %v, want z-migrations first: it is the only skill that mentions migration", slugs(got))
	}
}

// A term most candidates carry cannot decide between them, and one that only a few
// carry is why those few are here at all. Weighting every matched term the same is
// how a skill takes another's objectives by being wordy rather than by being right.
func TestRankWeighsARareTermAboveACommonOne(t *testing.T) {
	var cands []state.Skill
	for _, slug := range []string{"a-common", "b-common", "c-common", "d-common"} {
		cands = append(cands, state.Skill{
			Slug: slug, Name: slug,
			Description: "Everything about the service and the database.",
		})
	}
	// Carries one term the others do not, and one fewer of the terms they share.
	rare := state.Skill{Slug: "e-rare", Name: "e-rare", Description: "Sharding the database."}
	cands = append(cands, rare)

	terms := skillrecall.Keywords("sharding the database service")
	if got := skillrecall.Rank(terms, cands, 0); got[0].Slug != "e-rare" {
		t.Errorf("ranked %v; want e-rare first, since sharding separates it and database does not", slugs(got))
	}
}

func slugs(skills []state.Skill) []string {
	out := make([]string, len(skills))
	for i, s := range skills {
		out[i] = s.Slug
	}
	return out
}

// A skill with no description falls back to the head of its body, which is how a
// skill the distiller minted stays reachable. It is a fallback and not a policy: the
// head of a procedure is a poor account of when to reach for it.
func TestOfferFallsBackToTheBody(t *testing.T) {
	learned := state.Skill{Slug: "learned", Name: "learned", Body: "  Run the migration in two passes.  "}
	if got := skillrecall.Offer(learned); got != "Run the migration in two passes." {
		t.Errorf("Offer = %q, want the trimmed head of the body", got)
	}
	long := state.Skill{Slug: "long", Body: strings.Repeat("x", 500)}
	if got := skillrecall.Offer(long); len(got) > 260 {
		t.Errorf("the body fallback ran to %d characters; it is meant to be bounded", len(got))
	}
	described := state.Skill{Slug: "d", Description: "The description.", Body: "The body."}
	if got := skillrecall.Offer(described); got != "The description." {
		t.Errorf("Offer = %q, want the description whenever there is one", got)
	}
}

// A store that cannot answer is a miss, not a failure. Recall enriches a prompt, so
// a search backend having a bad day means the run is offered less rather than
// stopped, and the terms that do answer still count.
func TestGatherTreatsASearchErrorAsAMiss(t *testing.T) {
	store := failingSearch{library(t, debugging())}
	got := skillrecall.Gather(context.Background(), store, []string{"failing", "bisecting"}, 0)
	if len(got) != 1 || got[0].Slug != "systematic-debugging" {
		t.Fatalf("Gather = %+v, want the one skill the answering term found", got)
	}
}

// failingSearch answers the first query with an error and the rest normally.
type failingSearch struct{ state.SkillStore }

func (f failingSearch) Search(ctx context.Context, query string, limit int) ([]state.Skill, error) {
	if query == "failing" {
		return nil, context.DeadlineExceeded
	}
	return f.SkillStore.Search(ctx, query, limit)
}

func TestReportNamesEveryFailure(t *testing.T) {
	table, err := skillrecall.ParseTable([]byte("the tests are failing | absent-skill |"))
	if err != nil {
		t.Fatal(err)
	}
	report := skillrecall.Report(table.Check(context.Background(), library(t, debugging()), 0))
	if !strings.Contains(report, "absent-skill") || !strings.Contains(report, "line 1") {
		t.Errorf("report = %q, want the skill and the line to fix", report)
	}
}
