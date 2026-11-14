// Package recallplan turns a natural-language query into the ordered list of
// lexical matches a store should try, so every backend answers the same question
// the same way.
//
// A query used as one phrase finds an item only when its words sit next to each
// other, in order. A question such as "what did we decide about the database"
// then returns nothing from a corpus that plainly answers it, and an empty result
// looks the same as nothing stored, so the agent proceeds without the fact. The
// plan is strict first: every term must match, then only the content words, and
// only when both come back empty does it widen to any one of the content words.
// Strict-first is what keeps the widened read from returning a pile of one-word
// hits whenever a better answer exists.
//
// The planner is pure and knows no query language. A backend maps each [Attempt]
// onto its own syntax, runs them in order, and stops at the first that yields a
// result.
package recallplan

import (
	"slices"
	"strings"
	"unicode"
)

// Attempt is one lexical match to try.
type Attempt struct {
	// Terms are the words to match, lower-cased, without surrounding punctuation
	// and without duplicates.
	Terms []string

	// Any reports that an item matching at least one of Terms qualifies. When
	// false every one of Terms must match, at any position.
	Any bool
}

// Plan returns the attempts for query, in the order to try them. A backend runs
// the first, and runs the next only when the one before it produced no result.
//
// An empty query has no attempts: there is nothing to match, which is a different
// answer from "match everything", and the caller decides which it means. A query
// with no word in it, only punctuation, is matched literally as one term, as it
// always was, since there are no words to split it into. A single word has one
// attempt, since there is nothing to widen.
func Plan(query string) []Attempt {
	all := Terms(query)
	switch len(all) {
	case 0:
		if lit := strings.ToLower(strings.TrimSpace(query)); lit != "" {
			return []Attempt{{Terms: []string{lit}}}
		}
		return nil
	case 1:
		return []Attempt{{Terms: all}}
	}
	content := withoutStopWords(all)
	plan := []Attempt{{Terms: all}}
	if len(content) < len(all) {
		plan = append(plan, Attempt{Terms: content})
	}
	// Any of a single content word is the same match as all of it, so the widened
	// step is only a different attempt when there are at least two to choose from.
	if len(content) > 1 {
		plan = append(plan, Attempt{Terms: content, Any: true})
	}
	return plan
}

// Terms splits query into the distinct words a lexical match is made of, lower
// cased and trimmed of surrounding punctuation, in first-seen order. A token with
// no letter or digit in it is dropped, because it has nothing to match.
func Terms(query string) []string {
	var out []string
	for _, f := range strings.Fields(query) {
		w := strings.ToLower(strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		}))
		if w == "" || slices.Contains(out, w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// Hit is an item a plan selected.
type Hit[T any] struct {
	Item T

	// Terms are the terms of the attempt that selected Item.
	Terms []string

	// Share is the fraction of Terms the item holds, in (0,1]. It is 1 for every
	// hit of an attempt that requires all its terms, and below 1 only for a partial
	// match found by widening, which is how a full match outranks a partial one.
	Share float64
}

// Select applies the plan for query to items with no index to consult, by
// case-insensitive substring over text(item). It returns the hits of the first
// attempt that has any hit whose Share reaches floor, in the order of items, and
// nil when no attempt does. It serves a store that scans, and is the reference a
// backend with an index is checked against.
//
// The floor is applied before deciding to widen: a strict attempt whose every hit
// the caller would then discard has answered nothing.
func Select[T any](query string, items []T, text func(T) string, floor float64) []Hit[T] {
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = strings.ToLower(text(it))
	}
	for _, a := range Plan(query) {
		var hits []Hit[T]
		for i, it := range items {
			n := 0
			for _, t := range a.Terms {
				if strings.Contains(texts[i], t) {
					n++
				}
			}
			if n < a.required() {
				continue
			}
			share := float64(n) / float64(len(a.Terms))
			if share >= floor {
				hits = append(hits, Hit[T]{Item: it, Terms: a.Terms, Share: share})
			}
		}
		if len(hits) > 0 {
			return hits
		}
	}
	return nil
}

func (a Attempt) required() int {
	if a.Any {
		return 1
	}
	return len(a.Terms)
}

// withoutStopWords drops the words a question wraps around its subject. If that
// would drop every word, the query is returned whole: a query made only of such
// words is still a query.
func withoutStopWords(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if !stopWords[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return terms
	}
	return out
}

// stopWords are English function words and question words. They occur in nearly
// every item, so as alternatives they would admit the whole corpus.
var stopWords = map[string]bool{
	"a": true, "about": true, "after": true, "all": true, "also": true, "am": true,
	"an": true, "and": true, "any": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "but": true, "by": true, "can": true, "could": true,
	"did": true, "do": true, "does": true, "for": true, "from": true, "had": true,
	"has": true, "have": true, "how": true, "i": true, "if": true, "in": true,
	"into": true, "is": true, "it": true, "its": true, "me": true, "my": true,
	"of": true, "on": true, "or": true, "our": true, "should": true, "so": true,
	"that": true, "the": true, "their": true, "them": true, "then": true,
	"there": true, "these": true, "they": true, "this": true, "to": true,
	"us": true, "was": true, "we": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "who": true, "why": true, "will": true,
	"with": true, "would": true, "you": true, "your": true,
}
