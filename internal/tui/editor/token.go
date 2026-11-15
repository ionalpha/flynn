package editor

import "unicode"

// Token returns the text after the trigger in the token ending at the cursor.
// The token is the unbroken run of non-space atoms ending there and counts only
// when it starts with the trigger: "@src/f" completes, "user@host" does not. The
// query may be empty.
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

// CompleteToken replaces the active token with the trigger, text and a trailing
// space. Without an active token it does nothing, so a stale accept cannot
// splice the wrong range. One undo step restores the typed query.
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

// tokenStart finds the start of the run of non-space atoms ending at the cursor
// and reports whether it begins with the trigger. Chips bound the run like
// spaces.
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

// isSpaceAtom reports whitespace; tabs never enter the buffer.
func isSpaceAtom(a atom) bool {
	if a.opaque() {
		return false
	}
	for _, r := range a.text {
		return unicode.IsSpace(r)
	}
	return false
}
