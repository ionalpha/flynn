package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/skill/skillrecall"
	"github.com/ionalpha/flynn/state"
)

// recallLimit caps how many learned skills and memory items are injected into a
// run's prompt. It is deliberately small: recall is precision-first, since a long,
// loosely-relevant context degrades the model's use of it more than it helps.
const recallLimit = skillrecall.DefaultLimit

// recallContext queries the durable skills and memory for what is relevant to the
// objective and renders a compact, bounded block to prepend to the system prompt.
// It returns "" when nothing is on file, so a fresh agent's prompt is unchanged.
//
// Which skills an objective is offered is skillrecall's decision, not this file's.
// It is a package of its own so the pack's own test can ask what an objective would
// be offered without starting a run: a description that does not reach its subject
// is otherwise a silent failure, and the same ranker answering both is what makes
// that test mean anything. Memory is ranked here, on the same lexical cut.
//
// What a surviving skill contributes to the prompt is its name and its description,
// which is the discovery stage of the Agent Skills disclosure model and the whole of
// what a skill says about itself before something has decided it applies. The
// procedure is not here: the model calls skill_read for it. So the prompt carries a
// complete offer at roughly the cost of the truncated one it replaced, and a skill
// that is read leaves a record saying so.
//
// It returns the prompt block, the ids of the skills surfaced (for
// outcome reinforcement), and a compact human-readable line per recalled item (a
// skill name or a memory snippet) so the session can show the user what it pulled in.
func recallContext(ctx context.Context, skills state.SkillStore, memories state.MemoryStore, objective string) (block string, recalled []string, items []string) {
	terms := skillrecall.Keywords(objective)
	if len(terms) == 0 {
		return "", nil, nil
	}
	sk := skillrecall.Recall(ctx, skills, objective, recallLimit)
	mem := rankMemory(terms, gatherMemory(ctx, memories, terms))
	if len(sk) == 0 && len(mem) == 0 {
		return "", nil, nil
	}

	var b strings.Builder
	// Not "from earlier runs": recall is scope-blind, so a skill here may be one this
	// binary ships rather than one this install learned, and telling the model it
	// learned something it was given is a false claim in its own standing context.
	b.WriteString("The following skills and memory are available to you. Use anything relevant; ignore the rest.")
	if len(sk) > 0 {
		// The offer, and only the offer. Each line is a name and the whole of what the
		// skill is for, which is the stage the specification says activation keys on;
		// the procedure itself is fetched with skill_read once the model has decided a
		// skill applies. Pasting bodies here would spend the activation budget on skills
		// the run turns out not to need, and would leave no record of which ones it used.
		b.WriteString("\nSkills. Call skill_read with a skill's name to load its full procedure before acting on it:")
		for _, s := range sk {
			fmt.Fprintf(&b, "\n- %s: %s", s.Slug, skillrecall.Offer(s))
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

// rankMemory orders candidate memory items by relevance, most-recent first on a
// tie, then caps the result.
func rankMemory(terms []string, cands []state.MemoryItem) []state.MemoryItem {
	type scored struct {
		m     state.MemoryItem
		score int
	}
	ss := make([]scored, len(cands))
	for i, m := range cands {
		ss[i] = scored{m, skillrecall.MatchScore(terms, strings.ToLower(m.Content))}
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

// truncate shortens s to at most n runes, appending an ellipsis when it cut.
func truncate(s string, n int) string { return text.Clip(strings.TrimSpace(s), n) }
