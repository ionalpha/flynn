// Package recallplan turns a natural-language query into the ordered lexical
// matches a store tries, so every backend answers a query the same way.
//
// A query used as one phrase matches only adjacent words in order, so "what did we
// decide about the database" finds nothing in a corpus that answers it, and empty
// looks like nothing stored. The plan requires every term, then only the content
// words, and only when both are empty widens to any content word. Going strict
// first keeps the widened read from burying a better answer in one-word hits.
//
// A backend maps each [Attempt] onto its own syntax, runs them in order and stops
// at the first that yields a result.
package recallplan

import (
	"slices"
	"strings"
	"unicode"
)

// Attempt is one lexical match to try.
type Attempt struct {
	// Terms are the words to match: lower-cased, trimmed of punctuation, deduplicated.
	Terms []string

	// Any admits an item matching one of Terms; false requires all of them, at any
	// position.
	Any bool
}

// Plan returns the attempts for query in order; run the next only when the one
// before found nothing. An empty query has no attempts, because "nothing to match"
// is not "match everything" and the caller decides which it means. A query of only
// punctuation is one literal term, and a single word has one attempt.
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
	// Any of one content word equals all of it, so widening needs at least two.
	if len(content) > 1 {
		plan = append(plan, Attempt{Terms: content, Any: true})
	}
	return plan
}

// Terms splits query into distinct lower-cased words trimmed of punctuation, in
// first-seen order. A token with no letter or digit is dropped.
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

	// Share is the fraction of Terms the item holds, in (0,1]. Only a widened partial
	// match is below 1, which ranks full matches first.
	Share float64
}

// Select applies the plan to items by case-insensitive substring over text(item),
// for a store with no index, and is the reference an indexed backend is checked
// against. It returns the first attempt's hits whose Share reaches floor, in item
// order, or nil. The floor applies before widening, because a strict attempt whose
// hits all fall below it has answered nothing.
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

// withoutStopWords drops the words a question wraps around its subject, or returns
// terms whole when all are stop words.
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

// stopWords are English function and question words. They occur in nearly every
// item, so as alternatives they would admit the whole corpus.
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
