// Package skillrecall decides which skills an objective is offered, and lets that
// decision be tested without a model.
//
// This is stage one of the Agent Skills disclosure model: the runtime has an
// objective and a library, and it has to choose the few skills whose name and
// description go into the prompt. Everything after that (whether the model reads a
// body, whether the run then succeeds) depends on a skill being here first, so a
// description that does not reach its own subject makes the skill invisible without
// erroring, failing a test, or changing anything a run reports.
//
// It is its own package for that reason. The ranker used to live inside the CLI,
// where the only way to ask what an objective would be offered was to start a run;
// here the pack's own test can put an objective in and read the offer out, in
// milliseconds, with no tokens spent. See Table for the form that check takes.
package skillrecall

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// DefaultLimit caps how many skills an objective is offered. It is deliberately
// small: recall is precision-first, since a long, loosely-relevant context degrades
// the model's use of it more than it helps.
const DefaultLimit = 5

// fallbackOfferLen bounds the offer built from a body when a skill has no
// description. It is the length the prompt carried for every skill before
// descriptions existed, kept so the fallback costs what it always did.
const fallbackOfferLen = 240

// candidatePool multiplies the offer limit to size each per-term search. A store
// answers a term with its best matches capped at what it was asked for, so asking
// for exactly the offer limit hands ranking a set that was already cut by the
// store's own tie-break. Below that cut sits every skill that shares the term and
// sorted later, which for a common word is most of a growing library. Gathering
// wider costs one bounded query per term and gives Rank something to choose from.
const candidatePool = 4

// Recall returns the skills an objective is offered, best first, capped at limit
// (limit <= 0 uses DefaultLimit). It is the whole of stage one: the objective's
// content words become queries, the store's search answers each, and the union is
// ranked.
//
// A store error is a miss rather than a failure. Recall is an enrichment: a run
// whose search backend is unhappy is offered less, not stopped.
func Recall(ctx context.Context, skills state.SkillStore, objective string, limit int) []state.Skill {
	terms := Keywords(objective)
	if len(terms) == 0 {
		return nil
	}
	return Rank(terms, Gather(ctx, skills, terms, limit), limit)
}

// Gather unions the per-keyword search hits into a deduped candidate set for
// ranking. The store's full-text search matches a query as a single phrase, so
// recall runs one query per keyword rather than one query for the objective.
func Gather(ctx context.Context, skills state.SkillStore, terms []string, limit int) []state.Skill {
	if limit <= 0 {
		limit = DefaultLimit
	}
	seen := map[string]bool{}
	var out []state.Skill
	for _, term := range terms {
		found, err := skills.Search(ctx, term, limit*candidatePool)
		if err != nil {
			continue
		}
		for _, s := range found {
			if !seen[s.ID] {
				seen[s.ID] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// Rank orders candidate skills by relevance, then caps the result at limit
// (limit <= 0 uses DefaultLimit). Relevance is decided first and on its own;
// verification and a confirmed track record break ties between candidates the
// objective's words cannot separate, and never outrank a better match.
//
// A term is worth what it discriminates. Counting matched terms equally lets a
// skill win on a word most of the library carries, which is how one skill takes
// another's objectives simply by being wordy, so each term is weighted by how few
// of the candidates carry it.
func Rank(terms []string, cands []state.Skill, limit int) []state.Skill {
	if limit <= 0 {
		limit = DefaultLimit
	}
	type scored struct {
		s         state.Skill
		relevance float64
		evidence  float64
	}
	// Scored over the text the offer will carry, not over the body. A skill ranked
	// on words the model never sees produces an offer that reads as irrelevant to
	// the objective that surfaced it.
	hays := make([]string, len(cands))
	for i, s := range cands {
		hays[i] = strings.ToLower(s.Slug + " " + s.Name + " " + Offer(s) + " " + strings.Join(s.Tags, " "))
	}
	weights := termWeights(terms, hays)

	ss := make([]scored, len(cands))
	for i, s := range cands {
		relevance := 0.0
		for _, t := range terms {
			if strings.Contains(hays[i], t) {
				relevance += weights[t]
			}
		}
		ss[i] = scored{s, relevance, float64(verifiedBoost(s.Tags)) + learn.Confidence(s.Reads, s.Wins)}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].relevance != ss[j].relevance {
			return ss[i].relevance > ss[j].relevance
		}
		if ss[i].evidence != ss[j].evidence {
			return ss[i].evidence > ss[j].evidence
		}
		return ss[i].s.Slug < ss[j].s.Slug
	})
	out := make([]state.Skill, 0, limit)
	for _, x := range ss {
		if len(out) >= limit {
			break
		}
		out = append(out, x.s)
	}
	return out
}

// Offer returns what a skill says about itself at discovery: its description, which
// the format requires and bounds, and which is written to be exactly this - the
// statement of what the skill is for and when to reach for it.
//
// A skill the distiller minted has none, and falls back to the head of its body.
// That is the old behaviour, kept so a learned skill stays recallable rather than
// because it is any good: the head of a procedure is a poor account of when to reach
// for it. It stops being needed once capture writes a description of its own.
func Offer(s state.Skill) string {
	if d := strings.TrimSpace(s.Description); d != "" {
		return text.Clip(d, skillmd.MaxDescriptionLen)
	}
	return text.Clip(strings.TrimSpace(s.Body), fallbackOfferLen)
}

// termWeights gives each term what a match on it is worth: the fewer candidates
// carry it, the more it says about the ones that do. A term every candidate has
// separates nobody and is worth least; a term one candidate has is why that
// candidate is here at all. Every weight stays above zero, so a skill matching
// more of the objective still beats one matching less of it.
//
// The document frequency is counted over the gathered candidates rather than the
// whole library, because those are the skills being chosen between and it needs no
// second pass over the store.
func termWeights(terms []string, hays []string) map[string]float64 {
	weights := make(map[string]float64, len(terms))
	for _, t := range terms {
		df := 0
		for _, h := range hays {
			if strings.Contains(h, t) {
				df++
			}
		}
		if df == 0 {
			weights[t] = 0
			continue
		}
		weights[t] = math.Log(1 + float64(len(hays))/float64(df))
	}
	return weights
}

// MatchScore counts how many distinct terms appear in text, the lexical relevance
// signal recall ranks on. The text is expected lowercased, as the terms are.
func MatchScore(terms []string, text string) int {
	n := 0
	for _, t := range terms {
		if strings.Contains(text, t) {
			n++
		}
	}
	return n
}

// verifiedBoost nudges a skill whose check passed (tagged verified) above an
// otherwise equally relevant unverified one, so evidence breaks ties.
func verifiedBoost(tags []string) int {
	for _, t := range tags {
		if t == "verified" {
			return 1
		}
	}
	return 0
}

// stopwords are common words dropped from an objective before recall, so a query
// term carries signal rather than matching nearly everything.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"into": true, "from": true, "your": true, "you": true, "use": true, "run": true,
	"add": true, "all": true, "are": true, "its": true, "out": true, "via": true,
}

// maxKeywords caps how many content words of an objective become query terms.
const maxKeywords = 8

// Keywords reduces an objective to up to eight distinct, lowercased content words
// (alphanumeric, 3+ characters, not a stopword) used as recall query terms.
func Keywords(s string) []string {
	seen := map[string]bool{}
	var out []string
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, f := range fields {
		if len(f) < 3 || stopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) >= maxKeywords {
			break
		}
	}
	return out
}
