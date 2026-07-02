package editor

import "github.com/ionalpha/flynn/internal/tui/input"

// Action is what the caller should do after the editor consumed an event.
type Action int

const (
	// ActionNone reports the event was not the editor's to handle.
	ActionNone Action = iota
	// ActionRedraw reports the buffer or cursor changed; repaint the composer.
	ActionRedraw
	// ActionSubmit reports the user pressed Enter; send Content and Clear.
	ActionSubmit
	// ActionEsc reports Escape with the buffer untouched by this key; the
	// caller owns what Esc means (backtrack, dismiss an overlay, interrupt).
	ActionEsc
	// ActionTab hands completion to the caller, since it needs the file and
	// command universe the editor has no business knowing.
	ActionTab
	// ActionHistoryPrev reports the cursor tried to move above the first
	// line; recall the previous prompt.
	ActionHistoryPrev
	// ActionHistoryNext reports the cursor tried to move below the last
	// line; recall the next prompt.
	ActionHistoryNext
)

// Handle consumes one decoded input event. The bindings are the emacs and
// readline set every shell user already knows, plus the arrow, Home/End,
// and Delete keys in both their legacy and kitty encodings (the input
// decoder has already normalized those onto one vocabulary).
func (e *Editor) Handle(ev input.Event) Action {
	switch ev := ev.(type) {
	case input.Paste:
		e.InsertPaste(ev.Text)
		return ActionRedraw
	case input.Key:
		return e.key(ev)
	}
	return ActionNone
}

func (e *Editor) key(k input.Key) Action {
	switch k.Code {
	case input.KeyEnter:
		// Plain Enter submits; Alt+Enter and Shift+Enter insert a line
		// break (Shift+Enter only reaches us on kitty-protocol terminals;
		// Ctrl+J below covers the rest).
		if k.Mods&(input.ModAlt|input.ModShift) != 0 {
			e.Insert("\n")
			return ActionRedraw
		}
		return ActionSubmit
	case input.KeyEsc:
		return ActionEsc
	case input.KeyTab:
		return ActionTab
	case input.KeyBackspace:
		if k.Mods&(input.ModCtrl|input.ModAlt) != 0 {
			e.KillWordBack()
		} else {
			e.Backspace()
		}
		return ActionRedraw
	case input.KeyDelete:
		e.Delete()
		return ActionRedraw
	case input.KeyLeft:
		if k.Mods&(input.ModCtrl|input.ModAlt) != 0 {
			e.WordLeft()
		} else {
			e.Left()
		}
		return ActionRedraw
	case input.KeyRight:
		if k.Mods&(input.ModCtrl|input.ModAlt) != 0 {
			e.WordRight()
		} else {
			e.Right()
		}
		return ActionRedraw
	case input.KeyUp:
		if !e.Up() {
			return ActionHistoryPrev
		}
		return ActionRedraw
	case input.KeyDown:
		if !e.Down() {
			return ActionHistoryNext
		}
		return ActionRedraw
	case input.KeyHome:
		e.LineStart()
		return ActionRedraw
	case input.KeyEnd:
		e.LineEnd()
		return ActionRedraw
	}

	switch k.Mods {
	case input.ModCtrl:
		return e.ctrlKey(k.Code)
	case input.ModAlt:
		return e.altKey(k.Code)
	default:
	}

	if t := k.Text(); t != "" {
		e.Insert(t)
		return ActionRedraw
	}
	return ActionNone
}

func (e *Editor) ctrlKey(code rune) Action {
	switch code {
	case 'a':
		e.LineStart()
	case 'e':
		e.LineEnd()
	case 'b':
		e.Left()
	case 'f':
		e.Right()
	case 'd':
		e.Delete()
	case 'k':
		e.KillToEnd()
	case 'u':
		e.KillToStart()
	case 'w':
		e.KillWordBack()
	case 'y':
		e.Yank()
	case 'j':
		e.Insert("\n")
	case '_':
		e.Undo()
	default:
		return ActionNone
	}
	return ActionRedraw
}

func (e *Editor) altKey(code rune) Action {
	switch code {
	case 'b':
		e.WordLeft()
	case 'f':
		e.WordRight()
	case 'd':
		e.KillWordForward()
	case 'y':
		e.YankPop()
	case '_':
		e.Redo()
	default:
		return ActionNone
	}
	return ActionRedraw
}
