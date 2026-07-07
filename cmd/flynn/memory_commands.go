package main

import (
	"context"
	"fmt"
	"io"

	"github.com/ionalpha/flynn/state"
)

// renderSkills writes the learned skills to w: each one's name, whether it is
// verified, its outcome record (uses and wins), and a one-line preview of its body,
// so a user can see what the agent has learned and how well it has performed.
func renderSkills(ctx context.Context, w io.Writer, skills state.SkillStore) {
	list, err := skills.List(ctx, state.Scope{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "could not read skills: %v\n", err)
		return
	}
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, "no skills learned yet")
		return
	}
	_, _ = fmt.Fprintf(w, "%d learned skill(s):\n", len(list))
	for _, s := range list {
		verified := ""
		if hasTag(s.Tags, "verified") {
			verified = " [verified]"
		}
		_, _ = fmt.Fprintf(w, "  %s%s (used %d, won %d)\n", s.Name, verified, s.Uses, s.Wins)
		if body := oneLine(s.Body, 160); body != "" {
			_, _ = fmt.Fprintf(w, "    %s\n", body)
		}
	}
}

// renderMemory writes the durable memory items to w: each one's kind and a one-line
// preview of its content, so a user can see what the agent remembers across runs.
func renderMemory(ctx context.Context, w io.Writer, memories state.MemoryStore) {
	list, err := memories.Recall(ctx, state.RecallQuery{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "could not read memory: %v\n", err)
		return
	}
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, "no memory yet")
		return
	}
	_, _ = fmt.Fprintf(w, "%d memory item(s):\n", len(list))
	for _, m := range list {
		kind := m.Kind
		if kind == "" {
			kind = "fact"
		}
		_, _ = fmt.Fprintf(w, "  [%s] %s\n", kind, oneLine(m.Content, 160))
	}
}

// hasTag reports whether tags contains tag.
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
