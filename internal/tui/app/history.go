package app

// history is the prompt recall ring: submitted prompts, oldest first, plus
// the draft the user was typing when they first pressed up. Recall walks
// entries with prev and next; leaving the newest end restores the draft, so
// a half-typed prompt is never lost to a stray arrow key.
type history struct {
	entries []string
	// pos is the entry the composer currently shows; len(entries) means the
	// live draft.
	pos   int
	draft string
}

// add records a submitted prompt and resets recall to the live end.
// Consecutive duplicates collapse, matching every shell's history.
func (h *history) add(text string) {
	if n := len(h.entries); n == 0 || h.entries[n-1] != text {
		h.entries = append(h.entries, text)
	}
	h.pos = len(h.entries)
	h.draft = ""
}

// prev steps to the previous prompt, capturing the live draft on the first
// step back. It reports false at the oldest entry (or with no history).
func (h *history) prev(current string) (string, bool) {
	if h.pos == 0 {
		return "", false
	}
	if h.pos == len(h.entries) {
		h.draft = current
	}
	h.pos--
	return h.entries[h.pos], true
}

// next steps toward the live end, restoring the draft when it arrives there.
// It reports false when the composer already shows the draft.
func (h *history) next() (string, bool) {
	if h.pos == len(h.entries) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.pos], true
}
