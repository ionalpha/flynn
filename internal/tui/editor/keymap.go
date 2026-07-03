package editor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/internal/tui/input"
)

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

// Command names one editor operation a key chord can bind to. Names are
// dotted: "compose.*" commands resolve to caller-owned actions (submit,
// escape, completion), "editor.*" commands act on the buffer directly.
type Command string

// The bindable command vocabulary. CmdUnbind is the explicit "none": binding
// a chord to it removes the default binding for that chord.
const (
	CmdUnbind Command = "none"

	CmdSubmit   Command = "compose.submit"
	CmdNewline  Command = "compose.newline"
	CmdEscape   Command = "compose.escape"
	CmdComplete Command = "compose.complete"

	CmdBackspace       Command = "editor.backspace"
	CmdDelete          Command = "editor.delete"
	CmdDeleteOrEOF     Command = "editor.delete-or-eof"
	CmdLeft            Command = "editor.left"
	CmdRight           Command = "editor.right"
	CmdWordLeft        Command = "editor.word-left"
	CmdWordRight       Command = "editor.word-right"
	CmdCursorUp        Command = "editor.cursor-up"
	CmdCursorDown      Command = "editor.cursor-down"
	CmdLineStart       Command = "editor.line-start"
	CmdLineEnd         Command = "editor.line-end"
	CmdKillToEnd       Command = "editor.kill-to-end"
	CmdKillToStart     Command = "editor.kill-to-start"
	CmdKillWordBack    Command = "editor.kill-word-back"
	CmdKillWordForward Command = "editor.kill-word-forward"
	CmdYank            Command = "editor.yank"
	CmdYankPop         Command = "editor.yank-pop"
	CmdUndo            Command = "editor.undo"
	CmdRedo            Command = "editor.redo"
)

// commands is the dispatch table from a command name to its behaviour.
// Loading validates command names against this table, so a misspelled name
// in a keymap file is refused instead of silently binding nothing.
var commands = map[Command]func(*Editor) Action{
	CmdSubmit:   func(*Editor) Action { return ActionSubmit },
	CmdEscape:   func(*Editor) Action { return ActionEsc },
	CmdComplete: func(*Editor) Action { return ActionTab },
	CmdNewline: func(e *Editor) Action {
		e.Insert("\n")
		return ActionRedraw
	},
	CmdBackspace: func(e *Editor) Action { e.Backspace(); return ActionRedraw },
	CmdDelete:    func(e *Editor) Action { e.Delete(); return ActionRedraw },
	CmdDeleteOrEOF: func(e *Editor) Action {
		// Delete forward, except on an empty buffer, where the key is left
		// unclaimed for the session (the readline EOF convention).
		if e.Empty() {
			return ActionNone
		}
		e.Delete()
		return ActionRedraw
	},
	CmdLeft:      func(e *Editor) Action { e.Left(); return ActionRedraw },
	CmdRight:     func(e *Editor) Action { e.Right(); return ActionRedraw },
	CmdWordLeft:  func(e *Editor) Action { e.WordLeft(); return ActionRedraw },
	CmdWordRight: func(e *Editor) Action { e.WordRight(); return ActionRedraw },
	CmdCursorUp: func(e *Editor) Action {
		// Moving above the first line recalls history instead.
		if !e.Up() {
			return ActionHistoryPrev
		}
		return ActionRedraw
	},
	CmdCursorDown: func(e *Editor) Action {
		if !e.Down() {
			return ActionHistoryNext
		}
		return ActionRedraw
	},
	CmdLineStart:       func(e *Editor) Action { e.LineStart(); return ActionRedraw },
	CmdLineEnd:         func(e *Editor) Action { e.LineEnd(); return ActionRedraw },
	CmdKillToEnd:       func(e *Editor) Action { e.KillToEnd(); return ActionRedraw },
	CmdKillToStart:     func(e *Editor) Action { e.KillToStart(); return ActionRedraw },
	CmdKillWordBack:    func(e *Editor) Action { e.KillWordBack(); return ActionRedraw },
	CmdKillWordForward: func(e *Editor) Action { e.KillWordForward(); return ActionRedraw },
	CmdYank:            func(e *Editor) Action { e.Yank(); return ActionRedraw },
	CmdYankPop:         func(e *Editor) Action { e.YankPop(); return ActionRedraw },
	CmdUndo:            func(e *Editor) Action { e.Undo(); return ActionRedraw },
	CmdRedo:            func(e *Editor) Action { e.Redo(); return ActionRedraw },
}

// Keymap maps a normalized key chord (see ParseChord) to a command. Chords
// not in the map fall through: printable text inserts, everything else is
// unclaimed.
type Keymap map[string]Command

// Default is the built-in keymap: the emacs and readline set every shell
// user already knows, plus the arrow, Home/End, and Delete keys in both
// their legacy and kitty encodings (the input decoder has already
// normalized those onto one vocabulary).
func Default() Keymap {
	return Keymap{
		// Plain Enter submits; Alt+Enter and Shift+Enter insert a line
		// break (Shift+Enter only reaches us on kitty-protocol terminals;
		// Ctrl+J covers the rest).
		"enter":       CmdSubmit,
		"alt+enter":   CmdNewline,
		"shift+enter": CmdNewline,
		"ctrl+j":      CmdNewline,

		"esc": CmdEscape,
		"tab": CmdComplete,

		"backspace":      CmdBackspace,
		"ctrl+backspace": CmdKillWordBack,
		"alt+backspace":  CmdKillWordBack,
		"delete":         CmdDelete,

		"left":       CmdLeft,
		"right":      CmdRight,
		"ctrl+left":  CmdWordLeft,
		"alt+left":   CmdWordLeft,
		"ctrl+right": CmdWordRight,
		"alt+right":  CmdWordRight,
		"up":         CmdCursorUp,
		"down":       CmdCursorDown,
		"home":       CmdLineStart,
		"end":        CmdLineEnd,

		"ctrl+a": CmdLineStart,
		"ctrl+e": CmdLineEnd,
		"ctrl+b": CmdLeft,
		"ctrl+f": CmdRight,
		"ctrl+d": CmdDeleteOrEOF,
		"ctrl+k": CmdKillToEnd,
		"ctrl+u": CmdKillToStart,
		"ctrl+w": CmdKillWordBack,
		"ctrl+y": CmdYank,
		"ctrl+_": CmdUndo,

		"alt+b": CmdWordLeft,
		"alt+f": CmdWordRight,
		"alt+d": CmdKillWordForward,
		"alt+y": CmdYankPop,
		"alt+_": CmdRedo,
	}
}

// keyNames maps chord key-name spellings to functional key codes.
var keyNames = map[string]rune{
	"enter":     input.KeyEnter,
	"esc":       input.KeyEsc,
	"tab":       input.KeyTab,
	"backspace": input.KeyBackspace,
	"delete":    input.KeyDelete,
	"up":        input.KeyUp,
	"down":      input.KeyDown,
	"left":      input.KeyLeft,
	"right":     input.KeyRight,
	"home":      input.KeyHome,
	"end":       input.KeyEnd,
	"space":     ' ',
}

// nameOf is keyNames inverted, for formatting a chord from a decoded key.
var nameOf = func() map[rune]string {
	m := make(map[rune]string, len(keyNames))
	for name, code := range keyNames {
		m[code] = name
	}
	return m
}()

// modNames lists modifier spellings in canonical chord order.
var modNames = []struct {
	name string
	bit  input.Mod
}{
	{"ctrl", input.ModCtrl},
	{"alt", input.ModAlt},
	{"shift", input.ModShift},
	{"super", input.ModSuper},
}

// ParseChord normalizes one chord spelling ("Ctrl+Shift+Left", "alt+d",
// "enter") into the canonical form Keymap keys use: lowercase, modifiers in
// ctrl-alt-shift-super order, then a key name or a single character. It
// refuses spellings it cannot represent, so a typo in a keymap file is an
// error, not a binding that never fires.
func ParseChord(s string) (string, error) {
	parts := strings.Split(s, "+")
	// A trailing "+" means the key itself is "+" ("ctrl++" splits into
	// ["ctrl", "", ""]); rejoin that spelling into one literal key.
	if n := len(parts); n >= 2 && parts[n-1] == "" && parts[n-2] == "" {
		parts = append(parts[:n-2], "+")
	}
	var mods input.Mod
	for _, p := range parts[:len(parts)-1] {
		found := false
		for _, m := range modNames {
			if strings.EqualFold(p, m.name) {
				if mods&m.bit != 0 {
					return "", fmt.Errorf("keymap: chord %q repeats %q", s, m.name)
				}
				mods |= m.bit
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("keymap: chord %q: unknown modifier %q", s, p)
		}
	}
	key := parts[len(parts)-1]
	lower := strings.ToLower(key)
	if _, ok := keyNames[lower]; ok {
		return formatChord(mods, lower), nil
	}
	if r := []rune(key); len(r) == 1 {
		return formatChord(mods, strings.ToLower(key)), nil
	}
	return "", fmt.Errorf("keymap: chord %q: unknown key %q (named keys: %s)", s, key, knownKeyNames())
}

// chordOf renders a decoded key in the canonical chord form ParseChord
// produces, so map lookups and file spellings meet on one vocabulary.
func chordOf(k input.Key) string {
	if name, ok := nameOf[k.Code]; ok {
		return formatChord(k.Mods, name)
	}
	return formatChord(k.Mods, strings.ToLower(string(k.Code)))
}

func formatChord(mods input.Mod, key string) string {
	var b strings.Builder
	for _, m := range modNames {
		if mods&m.bit != 0 {
			b.WriteString(m.name)
			b.WriteByte('+')
		}
	}
	b.WriteString(key)
	return b.String()
}

func knownKeyNames() string {
	names := make([]string, 0, len(keyNames))
	for name := range keyNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// SetKeymap replaces the editor's bindings. Nil restores the default map.
// The map is used as given and must not be mutated afterwards.
func (e *Editor) SetKeymap(km Keymap) { e.keys = km }

// Handle consumes one decoded input event, resolving keys through the
// editor's keymap (the default map unless SetKeymap changed it).
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
	km := e.keys
	if km == nil {
		km = defaultKeymap
	}
	if cmd, ok := km[chordOf(k)]; ok {
		return commands[cmd](e)
	}
	// Some terminals report Shift on functional keys that have no plain
	// encoding of their own; an unbound shifted functional key falls back
	// to its unshifted binding rather than going unclaimed.
	if k.IsFunctional() && k.Mods&input.ModShift != 0 {
		unshifted := k
		unshifted.Mods &^= input.ModShift
		if cmd, ok := km[chordOf(unshifted)]; ok {
			return commands[cmd](e)
		}
	}
	if t := k.Text(); t != "" {
		e.Insert(t)
		return ActionRedraw
	}
	return ActionNone
}

// defaultKeymap is the shared map behind editors nobody called SetKeymap
// on; it is never mutated after init.
var defaultKeymap = Default()
