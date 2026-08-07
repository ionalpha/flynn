package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/state"
)

// recallLimit caps how many learned skills and memory items are injected into a
// run's prompt. It is deliberately small: recall is precision-first, since a long,
// loosely-relevant context degrades the model's use of it more than it helps.
const recallLimit = 5

// recallContext queries the durable skills and memory for what is relevant to the
// objective and renders a compact, bounded block to prepend to the system prompt.
// It returns "" when nothing is on file, so a fresh agent's prompt is unchanged.
//
// The store's full-text search matches a query as a single phrase, so recall runs
// one query per keyword of the objective, unions the hits, then ranks them by how
// many of the objective's keywords each one carries, with verified skills boosted
// above unverified ones. Only the top few survive, since a long, loosely-relevant
// context hurts the model's use of it more than it helps. This is a lexical first
// cut; vector recall is a later refinement.
// recallContext returns the prompt block, the ids of the skills surfaced (for
// outcome reinforcement), and a compact human-readable line per recalled item (a
// skill name or a memory snippet) so the session can show the user what it pulled in.
func recallContext(ctx context.Context, skills state.SkillStore, memories state.MemoryStore, objective string) (block string, recalled []string, items []string) {
	terms := keywords(objective)
	if len(terms) == 0 {
		return "", nil, nil
	}
	sk := rankSkills(terms, gatherSkills(ctx, skills, terms))
	mem := rankMemory(terms, gatherMemory(ctx, memories, terms))
	if len(sk) == 0 && len(mem) == 0 {
		return "", nil, nil
	}

	var b strings.Builder
	b.WriteString("From earlier runs you have learned the following. Use anything relevant; ignore the rest.")
	if len(sk) > 0 {
		b.WriteString("\nSkills:")
		for _, s := range sk {
			fmt.Fprintf(&b, "\n- %s: %s", s.Name, truncate(s.Body, 240))
			recalled = append(recalled, s.ID)
			items = append(items, "skill: "+s.Name)
		}
	}
	if len(mem) > 0 {
		b.WriteString("\nMemory:")
		for _, m := range mem {
			fmt.Fprintf(&b, "\n- %s", truncate(m.Content, 240))
			items = append(items, "memory: "+oneLine(m.Content, 100))
		}
	}
	return b.String(), recalled, items
}

// gatherSkills unions the per-keyword full-text hits into a deduped candidate set
// for ranking.
func gatherSkills(ctx context.Context, skills state.SkillStore, terms []string) []state.Skill {
	seen := map[string]bool{}
	var out []state.Skill
	for _, term := range terms {
		found, err := skills.Search(ctx, term, recallLimit)
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

// gatherMemory unions the per-keyword recall hits into a deduped candidate set.
func gatherMemory(ctx context.Context, memories state.MemoryStore, terms []string) []state.MemoryItem {
	seen := map[string]bool{}
	var out []state.MemoryItem
	for _, term := range terms {
		found, err := memories.Recall(ctx, state.RecallQuery{Query: term, Limit: recallLimit})
		if err != nil {
			continue
		}
		for _, m := range found {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// rankSkills orders candidate skills by relevance (how many of the objective's
// keywords each carries), boosted for verified skills and for those with a strong
// confirmed track record, then caps the result. Relevance dominates; verification
// and confidence break ties between similarly relevant skills.
func rankSkills(terms []string, cands []state.Skill) []state.Skill {
	type scored struct {
		s     state.Skill
		score float64
	}
	ss := make([]scored, len(cands))
	for i, s := range cands {
		text := strings.ToLower(s.Name + " " + s.Body + " " + strings.Join(s.Tags, " "))
		score := float64(matchScore(terms, text)+verifiedBoost(s.Tags)) + learn.Confidence(s.Uses, s.Wins)
		ss[i] = scored{s, score}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score > ss[j].score
		}
		return ss[i].s.Slug < ss[j].s.Slug
	})
	out := make([]state.Skill, 0, recallLimit)
	for _, x := range ss {
		if len(out) >= recallLimit {
			break
		}
		out = append(out, x.s)
	}
	return out
}

// rankMemory orders candidate memory items by relevance, most-recent first on a
// tie, then caps the result.
func rankMemory(terms []string, cands []state.MemoryItem) []state.MemoryItem {
	type scored struct {
		m     state.MemoryItem
		score int
	}
	ss := make([]scored, len(cands))
	for i, m := range cands {
		ss[i] = scored{m, matchScore(terms, strings.ToLower(m.Content))}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score > ss[j].score
		}
		return ss[i].m.CreatedAt.After(ss[j].m.CreatedAt)
	})
	out := make([]state.MemoryItem, 0, recallLimit)
	for _, x := range ss {
		if len(out) >= recallLimit {
			break
		}
		out = append(out, x.m)
	}
	return out
}

// matchScore counts how many distinct terms appear in text, the lexical relevance
// signal recall ranks on.
func matchScore(terms []string, text string) int {
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

// recallStopwords are common words dropped from an objective before recall, so a
// query term carries signal rather than matching nearly everything.
var recallStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"into": true, "from": true, "your": true, "you": true, "use": true, "run": true,
	"add": true, "all": true, "are": true, "its": true, "out": true, "via": true,
}

// keywords reduces an objective to up to eight distinct, lowercased content words
// (alphanumeric, 3+ chars, not a stopword) used as recall query terms.
func keywords(s string) []string {
	seen := map[string]bool{}
	var out []string
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, f := range fields {
		if len(f) < 3 || recallStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// truncate shortens s to at most n runes, appending an ellipsis when it cut.
func truncate(s string, n int) string { return text.Clip(strings.TrimSpace(s), n) }
