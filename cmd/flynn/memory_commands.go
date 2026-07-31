package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// rememberSource is the provenance stamped on a memory item the user pinned by hand.
// It carries the guard's user scheme, so TrustOf grades it Trusted rather than the
// Semi a distilled item gets, and the poison screen does not gate it.
const rememberSource = guard.SchemeUser + "session"

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
		// Pinned means the user themselves stated it. An item distilled from several
		// inputs is only pinned if every one of them was the user, which is what
		// TrustOfAll answers: one user source mixed with a fetched page is not a
		// fact the user pinned.
		pinned := ""
		if guard.TrustOfAll(m.Sources) == sandbox.TrustTrusted {
			pinned = " [pinned]"
		}
		_, _ = fmt.Fprintf(w, "  [%s]%s %s\n", kind, pinned, oneLine(m.Content, 160))
	}
}

// rememberFact pins a fact the user stated into durable memory, so it is recalled in
// later runs rather than waiting on the distiller to infer it. The item is stamped
// with a user-scheme source, which the guard grades as Trusted. It reports whether
// the fact was written, so a caller that echoes a prompt can stay quiet on a no-op.
func rememberFact(ctx context.Context, w io.Writer, memories state.MemoryStore, fact string) bool {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		_, _ = fmt.Fprintln(w, "usage: /remember <fact to keep across runs>")
		return false
	}
	if _, err := memories.Write(ctx, state.MemoryItem{
		Kind:    "fact",
		Content: fact,
		Sources: []string{rememberSource},
	}); err != nil {
		_, _ = fmt.Fprintf(w, "could not write memory: %v\n", err)
		return false
	}
	_, _ = fmt.Fprintf(w, "  remembered: %s\n", oneLine(fact, 160))
	return true
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
