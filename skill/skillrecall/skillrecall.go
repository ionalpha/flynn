// Package skillrecall chooses the skills an objective is offered, stage one of Agent
// Skills disclosure, and tests that choice without a model.
//
// A skill that is never offered is never read, and nothing in a run reports it. A
// pack test feeds objectives through the same ranker in milliseconds with no tokens
// spent; [Table] is the form of that test.
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

// DefaultLimit caps how many skills an objective is offered. It is small because a
// long, loosely relevant context hurts the model more than it helps.
const DefaultLimit = 5

// fallbackOfferLen bounds, in runes, the offer clipped from a body when a skill has
// no description.
const fallbackOfferLen = 240

// candidatePool multiplies the offer limit to size each per-term search. Asking for
// exactly the limit would let the store's tie-break cut the set before [Rank] sees
// it, and for a common word that cut drops most of the library.
const candidatePool = 4

// Recall returns the skills an objective is offered, best first, at most limit
// (limit <= 0 uses [DefaultLimit]). It searches each of the objective's [Keywords]
// and ranks the union. A store error drops that term, so a failing search offers
// less and never stops a run.
func Recall(ctx context.Context, skills state.SkillStore, objective string, limit int) []state.Skill {
	terms := Keywords(objective)
	if len(terms) == 0 {
		return nil
	}
	return Rank(terms, Gather(ctx, skills, terms, limit), limit)
}

// Gather unions per-term search hits, deduped. The store matches a query as one
// phrase, so each keyword is its own query.
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

// Rank orders candidates by relevance and returns at most limit (limit <= 0 uses
// [DefaultLimit]). Verification and confidence only break relevance ties. Each term
// is weighted by how few candidates carry it, so a wordy skill cannot take another's
// objectives on common words.
func Rank(terms []string, cands []state.Skill, limit int) []state.Skill {
	if limit <= 0 {
		limit = DefaultLimit
	}
	type scored struct {
		s         state.Skill
		relevance float64
		evidence  float64
	}
	// Scored on the offered text, because a match in the body is invisible to the
	// model and the offer would look irrelevant.
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

// Offer returns the text shown at discovery: the description, or the head of the
// body for a distilled skill without one. The head of a procedure says little about
// when to use it, but it keeps a learned skill recallable.
func Offer(s state.Skill) string {
	if d := strings.TrimSpace(s.Description); d != "" {
		return text.Clip(d, skillmd.MaxDescriptionLen)
	}
	return text.Clip(strings.TrimSpace(s.Body), fallbackOfferLen)
}

// termWeights weights each term by log(1 + N/df) over the gathered candidates, the
// set being chosen between, with no second pass over the store. A term any candidate
// carries stays above zero, so matching more terms still wins.
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

// MatchScore counts the terms that appear in text. text must be lowercased, as the
// terms are.
func MatchScore(terms []string, text string) int {
	n := 0
	for _, t := range terms {
		if strings.Contains(text, t) {
			n++
		}
	}
	return n
}

func verifiedBoost(tags []string) int {
	for _, t := range tags {
		if t == "verified" {
			return 1
		}
	}
	return 0
}

// stopwords would match nearly every skill.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"into": true, "from": true, "your": true, "you": true, "use": true, "run": true,
	"add": true, "all": true, "are": true, "its": true, "out": true, "via": true,
}

const maxKeywords = 8

// Keywords returns up to eight distinct lowercased words of three or more letters or
// digits, without stopwords.
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
