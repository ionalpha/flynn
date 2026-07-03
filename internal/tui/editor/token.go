package editor

import "unicode"

// Token support for completion: the composer completes the trigger-prefixed
// word being typed, so the shell needs to read that word and, on accept,
// replace it. The token is the unbroken run of non-space atoms ending at the
// cursor; it only counts when its first atom is the trigger itself, so
// "@src/f" completes while "user@host" does not (its run starts at 'u').

// Token returns the text after the trigger in the token ending at the
// cursor, and whether such a token is active. The query may be empty: a
// trigger just typed is an active token asking for the unfiltered universe.
func (e *Editor) Token(trigger rune) (string, bool) {
	start, ok := e.tokenStart(trigger)
	if !ok {
		return "", false
	}
	var b []byte
	for _, a := range e.atoms[start+1 : e.cur] {
		b = append(b, a.text...)
	}
	return string(b), true
}

// CompleteToken replaces the active trigger token with the trigger, the
// chosen text, and a trailing space, and puts the cursor after the space.
// Without an active token it does nothing, so a stale accept (the buffer
// changed under a queued key) cannot splice the wrong range. One undo step
// restores the typed query.
func (e *Editor) CompleteToken(trigger rune, text string) {
	start, ok := e.tokenStart(trigger)
	if !ok {
		return
	}
	e.pushUndo()
	replacement := graphemes(string(trigger) + text + " ")
	out := make([]atom, 0, start+len(replacement)+len(e.atoms)-e.cur)
	out = append(out, e.atoms[:start]...)
	out = append(out, replacement...)
	cur := len(out)
	out = append(out, e.atoms[e.cur:]...)
	e.atoms, e.cur = out, cur
	e.last = opNone
}

// tokenStart finds the start of the run of non-space, non-chip atoms ending
// at the cursor, and reports whether that run begins with the trigger. Chips
// bound the run like spaces do: a chip (paste or image) is opaque, not part
// of a word being typed.
func (e *Editor) tokenStart(trigger rune) (int, bool) {
	i := e.cur
	for i > 0 {
		a := e.atoms[i-1]
		if a.opaque() || isSpaceAtom(a) {
			break
		}
		i--
	}
	if i >= e.cur {
		return 0, false
	}
	first := e.atoms[i]
	if first.opaque() {
		return 0, false
	}
	rs := []rune(first.text)
	if len(rs) != 1 || rs[0] != trigger {
		return 0, false
	}
	return i, true
}

// isSpaceAtom reports whether the atom is whitespace (a space or a line
// break; tabs never enter the buffer).
func isSpaceAtom(a atom) bool {
	if a.opaque() {
		return false
	}
	for _, r := range a.text {
		return unicode.IsSpace(r)
	}
	return false
}
