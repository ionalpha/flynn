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
	// ActionNone means the event was not the editor's to handle.
	ActionNone Action = iota
	// ActionRedraw means the buffer or cursor changed.
	ActionRedraw
	// ActionSubmit means Enter: send Content and Clear.
	ActionSubmit
	// ActionEsc means Escape, whose meaning the caller owns.
	ActionEsc
	// ActionTab hands completion to the caller, which knows the file and command
	// universe.
	ActionTab
	// ActionHistoryPrev means the cursor tried to move above the first line.
	ActionHistoryPrev
	// ActionHistoryNext means the cursor tried to move below the last line.
	ActionHistoryNext
)

// Command names an operation a key chord binds to. "compose.*" commands resolve
// to caller-owned actions; "editor.*" commands act on the buffer.
type Command string

// The bindable commands. Binding a chord to CmdUnbind removes its default.
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

// commands is the dispatch table. Loading validates names against it, so a
// misspelled name is refused rather than binding nothing.
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
		// On an empty buffer the key is left to the session (readline EOF).
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

// Keymap maps a normalized chord (see [ParseChord]) to a command. Unmapped
// chords fall through: printable text inserts, the rest is unclaimed.
type Keymap map[string]Command

// Default is the emacs and readline keymap plus the arrow, Home/End and Delete
// keys; the input decoder has already normalized their encodings.
func Default() Keymap {
	return Keymap{
		// Shift+Enter only arrives on kitty-protocol terminals; Ctrl+J covers the rest.
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

var nameOf = func() map[rune]string {
	m := make(map[rune]string, len(keyNames))
	for name, code := range keyNames {
		m[code] = name
	}
	return m
}()

var modNames = []struct {
	name string
	bit  input.Mod
}{
	{"ctrl", input.ModCtrl},
	{"alt", input.ModAlt},
	{"shift", input.ModShift},
	{"super", input.ModSuper},
}

// ParseChord normalizes a spelling ("Ctrl+Shift+Left") to the Keymap key form:
// lowercase, modifiers in ctrl-alt-shift-super order, then a key name or single
// character. An unrepresentable spelling is an error, so a typo cannot become a
// binding that never fires.
func ParseChord(s string) (string, error) {
	parts := strings.Split(s, "+")
	// "ctrl++" splits into ["ctrl", "", ""]; rejoin it as a literal "+" key.
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

// SetKeymap replaces the bindings; nil restores the default. The map is used as
// given and must not be mutated afterwards.
func (e *Editor) SetKeymap(km Keymap) { e.keys = km }

// Handle consumes one decoded event, resolving keys through the keymap.
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
	// Some terminals report Shift on functional keys with no plain encoding; an
	// unbound one falls back to its unshifted binding.
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

// defaultKeymap is shared and never mutated after init.
var defaultKeymap = Default()
