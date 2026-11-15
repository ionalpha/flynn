package app

// history is the prompt recall ring: submitted prompts, oldest first, plus the
// draft held when recall began. Leaving the newest end restores the draft, so a
// half-typed prompt survives a stray arrow key.
type history struct {
	entries []string
	// pos is the entry the composer currently shows; len(entries) means the
	// live draft.
	pos   int
	draft string
}

// add records a prompt and resets recall. Consecutive duplicates collapse.
func (h *history) add(text string) {
	if n := len(h.entries); n == 0 || h.entries[n-1] != text {
		h.entries = append(h.entries, text)
	}
	h.pos = len(h.entries)
	h.draft = ""
}

// prev captures the live draft on the first step back and reports false at the
// oldest entry.
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

// next restores the draft on reaching the live end and reports false when the
// draft is already showing.
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
