// Package input decodes the terminal's raw byte stream into typed events: keys
// with modifiers, bracketed pastes and focus changes. Escape sequences and
// multi-byte characters split across reads are reassembled, and a paste arrives
// as one event.
//
// The decoder is a pure state machine over bytes (no reader, timers or terminal),
// so every sequence can be fed whole or byte by byte and must decode
// identically. A lone Escape is indistinguishable from the first byte of a
// sequence, so the caller's read loop waits briefly while Pending and calls
// Flush if nothing more arrives.
//
// It understands the kitty keyboard protocol (CSI u) and the legacy encodings
// (CSI and SS3 function keys, modifier parameters, control bytes, Alt as an
// Escape prefix), giving ConPTY, tmux and legacy emulators one event vocabulary.
package input

// Event is a [Key], [Paste], [Focus] or [Unknown].
type Event interface{ isEvent() }

// Mod is a bitmask of key modifiers.
type Mod uint8

// The modifier bits match the wire bitmask of the kitty and xterm encodings.
const (
	ModShift Mod = 1 << iota
	ModAlt
	ModCtrl
	ModSuper
)

// Key is one key press. Printable keys carry their rune in Code; functional keys
// use the Key* constants, private-use runes that cannot collide with text.
type Key struct {
	Code rune
	Mods Mod
}

func (Key) isEvent() {}

// Text returns the text this key inserts; empty for a functional or modified key.
func (k Key) Text() string {
	if k.IsFunctional() || k.Mods&(ModCtrl|ModAlt|ModSuper) != 0 || k.Code < ' ' {
		return ""
	}
	return string(k.Code)
}

// IsFunctional reports a Key* functional key. Code points above the block
// (emoji, supplementary-plane text) are ordinary text.
func (k Key) IsFunctional() bool {
	return k.Code >= keyBase && k.Code < keyLimit
}

// Paste is one complete bracketed paste with line endings normalized to \n.
type Paste struct{ Text string }

func (Paste) isEvent() {}

// Focus reports terminal focus changes (mode 1004).
type Focus struct{ Gained bool }

func (Focus) isEvent() {}

// Unknown carries an unmapped escape sequence, kept so a diagnostic view can
// show what the terminal sent.
type Unknown struct{ Seq string }

func (Unknown) isEvent() {}

const keyBase rune = 0xE000

// The functional keys every supported encoding normalizes onto.
const (
	KeyEnter rune = keyBase + iota
	KeyEsc
	KeyTab
	KeyBackspace
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyInsert
	KeyDelete
	KeyF1
	KeyF2
	KeyF3
	KeyF4
	KeyF5
	KeyF6
	KeyF7
	KeyF8
	KeyF9
	KeyF10
	KeyF11
	KeyF12

	// keyLimit ends the functional block and must stay last.
	keyLimit
)
