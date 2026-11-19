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
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

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
// (limit <= 0 uses [DefaultLimit]). It searches the objective's content words
// (see [Keywords]), ranks the union, and lets [Rank] keep the terms that tell the
// candidates apart. A store error drops that term, so a failing search offers less
// and never stops a run.
func Recall(ctx context.Context, skills state.SkillStore, objective string, limit int) []state.Skill {
	terms := contentWords(objective, maxProbe)
	if len(terms) == 0 {
		return nil
	}
	found := Gather(ctx, skills, terms, limit)
	cands := make([]Candidate, len(found))
	for i, s := range found {
		cands[i] = candidateOf(s)
	}
	picked := rank(terms, cands, limit)
	out := make([]state.Skill, len(picked))
	for i, at := range picked {
		out[i] = found[at]
	}
	return out
}

// Candidate is what [Rank] reads of a skill, so a host can rank without building
// storage values.
type Candidate struct {
	Slug string
	Name string
	// Offer is the text shown at discovery (see [Offer]). A term is scored against
	// this and not the body, because a match the model cannot see makes the offer
	// look irrelevant.
	Offer string
	Tags  []string
	// Evidence is how far the host trusts the skill. It only breaks ties between
	// equally relevant candidates and never outweighs a better match.
	Evidence float64
}

// candidateOf is Flynn's call site for [Rank]: evidence is a passing check (the
// "verified" tag) plus the confidence of the read and win record.
func candidateOf(s state.Skill) Candidate {
	return Candidate{
		Slug: s.Slug, Name: s.Name, Offer: Offer(s), Tags: s.Tags,
		Evidence: float64(verifiedBoost(s.Tags)) + learn.Confidence(s.Reads, s.Wins),
	}
}

// Gather unions per-term search hits, deduped. The store matches a query as one
// phrase, so each keyword is its own query, and it matches whole words as written,
// so each inflected form of a keyword (see [inflections]) is a query too: [Rank]
// folds endings, and a skill the store never returns is a skill it never sees.
func Gather(ctx context.Context, skills state.SkillStore, terms []string, limit int) []state.Skill {
	if limit <= 0 {
		limit = DefaultLimit
	}
	seen := map[string]bool{}
	var out []state.Skill
	for _, term := range terms {
		for _, form := range inflections(term) {
			found, err := skills.Search(ctx, form, limit*candidatePool)
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
	}
	return out
}

// inflections returns term and the forms [stem] folds with it, for a store that
// matches words as written: the stem with each ending added, with and without the
// final e that folding drops, and with a doubled final consonant before an ending
// that starts with a vowel. A term that is not a plain word is returned alone.
func inflections(term string) []string {
	b := stem(term)
	forms := []string{term}
	if b == term && !plainWord(term) {
		return forms
	}
	add := func(f string) {
		if !slices.Contains(forms, f) {
			forms = append(forms, f)
		}
	}
	add(b)
	for _, end := range []string{"e", "s", "es", "ed", "ing"} {
		add(b + end)
	}
	if n := len(b); n > 0 && !strings.ContainsRune("aeiou", rune(b[n-1])) {
		for _, end := range []string{"ed", "ing"} {
			add(b + b[n-1:] + end)
		}
	}
	return forms
}

func plainWord(w string) bool {
	for _, r := range w {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return w != ""
}

// Rank orders candidates by relevance and returns at most limit (limit <= 0 uses
// [DefaultLimit]). A term counts when it appears as a whole word of the candidate's
// slug, name, offer or tags (see [MatchScore]). Each term is weighted by how few
// candidates carry it, so a wordy skill cannot take another's objectives on common
// words, and only the eight heaviest terms score, so a long input is judged on the
// words that separate these candidates and not on the ones it opened with.
// Evidence breaks relevance ties.
func Rank(terms []string, cands []Candidate, limit int) []Candidate {
	picked := rank(terms, cands, limit)
	out := make([]Candidate, len(picked))
	for i, at := range picked {
		out[i] = cands[at]
	}
	return out
}

// rank returns indexes into cands, so a caller holding richer values than
// [Candidate] can map the order back without assuming slugs are unique.
func rank(terms []string, cands []Candidate, limit int) []int {
	if limit <= 0 {
		limit = DefaultLimit
	}
	hays := make([]string, len(cands))
	for i, c := range cands {
		hays[i] = strings.ToLower(c.Slug + " " + c.Name + " " + c.Offer + " " + strings.Join(c.Tags, " "))
	}
	terms = normalise(terms)
	weights := termWeights(terms, hays)
	terms = heaviest(terms, weights, maxKeywords)

	type scored struct {
		at        int
		relevance float64
	}
	ss := make([]scored, len(cands))
	for i := range cands {
		relevance := 0.0
		for _, t := range terms {
			if hasWord(hays[i], t) {
				relevance += weights[t]
			}
		}
		ss[i] = scored{i, relevance}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		if a.relevance != b.relevance {
			return a.relevance > b.relevance
		}
		if ca, cb := cands[a.at].Evidence, cands[b.at].Evidence; ca != cb {
			return ca > cb
		}
		return cands[a.at].Slug < cands[b.at].Slug
	})
	if len(ss) > limit {
		ss = ss[:limit]
	}
	out := make([]int, len(ss))
	for i, x := range ss {
		out[i] = x.at
	}
	return out
}

// normalise lowercases terms and drops empty and repeated ones, so a caller's
// literal cannot score one word twice.
func normalise(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// heaviest keeps the n terms with the greatest weight, in their given order among
// equals. A term no candidate carries weighs nothing, so it goes first.
func heaviest(terms []string, weights map[string]float64, n int) []string {
	if len(terms) <= n {
		return terms
	}
	ordered := append([]string(nil), terms...)
	sort.SliceStable(ordered, func(i, j int) bool { return weights[ordered[i]] > weights[ordered[j]] })
	return ordered[:n]
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
			if hasWord(h, t) {
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

// MatchScore counts the terms that appear in text as whole words (see [Keywords]
// for what a term is). text must be lowercased, as the terms are.
func MatchScore(terms []string, text string) int {
	n := 0
	for _, t := range terms {
		if hasWord(text, t) {
			n++
		}
	}
	return n
}

// hasWord reports whether term occurs in hay as a word, or as another inflection of
// one: "paste" matches "pasting" and "renamed" matches "rename" (see [stem]). Both
// arguments are lowercased by the caller.
func hasWord(hay, term string) bool {
	if term == "" {
		return false
	}
	return hasBounded(hay, term) || hasInflection(hay, term)
}

// hasInflection reports whether a word of hay folds to the same stem as term. Only a
// term made of letters and digits folds: a path or a file name has no inflected form,
// and it is matched as written by [hasBounded].
func hasInflection(hay, term string) bool {
	for _, r := range term {
		if !isWordRune(r) {
			return false
		}
	}
	want := stem(term)
	for _, w := range strings.FieldsFunc(hay, func(r rune) bool { return !isWordRune(r) }) {
		if stem(w) == want {
			return true
		}
	}
	return false
}

// stem folds the common English endings of an ASCII word, so that the forms of one
// word compare equal: a plural (s, es), a past tense (ed), a participle (ing), a
// dropped final e ("pasting" and "paste" both fold to "past") and a doubled final
// consonant ("running" and "run" both fold to "run"). Folding stops short of a stem
// under three letters, or an ing stem with no vowel ("string", "bring"), so a short
// word is not reduced to a fragment other words share. A word with any other
// character is returned as it is. Because a suffix is only ever removed from the end,
// a word never folds to the same stem as a longer word that merely contains it
// ("ion" and "integration").
func stem(w string) string {
	if !plainWord(w) {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
	case strings.HasSuffix(w, "es") && len(w) > 4 && sibilant(w[:len(w)-2]):
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "s") && len(w) > 3:
		w = w[:len(w)-1]
	}
	switch {
	case strings.HasSuffix(w, "ing") && len(w) > 5 && hasVowel(w[:len(w)-3]):
		w = w[:len(w)-3]
	case strings.HasSuffix(w, "ed") && len(w) > 4 && hasVowel(w[:len(w)-2]):
		w = w[:len(w)-2]
	}
	if strings.HasSuffix(w, "e") && len(w) > 4 {
		w = w[:len(w)-1]
	}
	if n := len(w); n > 3 && w[n-1] == w[n-2] && !strings.ContainsRune("aeiou", rune(w[n-1])) {
		w = w[:n-1]
	}
	return w
}

// sibilant reports whether a plural of w takes "es" and not "s".
func sibilant(w string) bool {
	return strings.HasSuffix(w, "s") || strings.HasSuffix(w, "x") || strings.HasSuffix(w, "z") ||
		strings.HasSuffix(w, "ch") || strings.HasSuffix(w, "sh")
}

func hasVowel(w string) bool { return strings.ContainsAny(w, "aeiouy") }

// hasBounded reports whether term occurs in hay with a word boundary at each end of
// the term whose own character is a letter or digit. Terms are as often paths and
// identifiers as words (`core/`, `render.js`), and an end that is punctuation has
// nothing to be a boundary of: `core/` must still match `core/animation`.
func hasBounded(hay, term string) bool {
	first, _ := utf8.DecodeRuneInString(term)
	last, _ := utf8.DecodeLastRuneInString(term)
	for from := 0; from <= len(hay)-len(term); {
		at := strings.Index(hay[from:], term)
		if at < 0 {
			return false
		}
		at += from
		end := at + len(term)
		before, _ := utf8.DecodeLastRuneInString(hay[:at])
		after, _ := utf8.DecodeRuneInString(hay[end:])
		leftOK := !isWordRune(first) || at == 0 || !isWordRune(before)
		rightOK := !isWordRune(last) || end == len(hay) || !isWordRune(after)
		if leftOK && rightOK {
			return true
		}
		from = at + 1
	}
	return false
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func verifiedBoost(tags []string) int {
	for _, t := range tags {
		if t == "verified" {
			return 1
		}
	}
	return 0
}

// stopwords are function words that carry no subject. Rarity cannot drop them: the
// candidates are gathered by the terms themselves, so every term has at least one
// carrier and "not" in a short objective would otherwise count as much as its topic.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"into": true, "from": true, "your": true, "you": true, "use": true, "run": true,
	"add": true, "all": true, "are": true, "its": true, "out": true, "via": true,
	"not": true, "yet": true, "but": true, "what": true, "should": true, "was": true,
	"were": true, "has": true, "have": true, "had": true, "can": true, "will": true,
	"our": true, "how": true, "when": true, "does": true, "did": true, "just": true,
	"any": true, "get": true, "got": true, "would": true, "could": true,
}

// maxKeywords is how many terms [Keywords] returns and how many [Rank] scores on.
const maxKeywords = 8

// maxProbe is how many content words [Recall] searches for. It is wider than
// maxKeywords so the terms that tell candidates apart can be chosen after the search,
// and bounded because each is one store query. Past it, a word the input says once
// and late is not searched for.
const maxProbe = 32

// Keywords returns up to eight distinct lowercased content words of three or more
// letters or digits, without stopwords. A word the text repeats comes first, since a
// repeated word is more likely its subject; words repeated equally keep their order.
func Keywords(s string) []string { return contentWords(s, maxKeywords) }

func contentWords(s string, limit int) []string {
	count := map[string]int{}
	var order []string
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, f := range fields {
		if len(f) < 3 || stopwords[f] {
			continue
		}
		if count[f] == 0 {
			order = append(order, f)
		}
		count[f]++
	}
	sort.SliceStable(order, func(i, j int) bool { return count[order[i]] > count[order[j]] })
	if len(order) > limit {
		order = order[:limit]
	}
	return order
}
