// Package input decodes the terminal's raw byte stream into typed events:
// keys with modifiers, bracketed pastes, and focus changes. It is the layer
// that guarantees the session never loses a keystroke to framing: escape
// sequences are reassembled across read-chunk boundaries, a paste arrives as
// one event rather than a burst of keys, and multi-byte characters split by
// the read buffer are held until complete.
//
// The decoder is a pure state machine over bytes: no reader, no timers, no
// terminal. That keeps it exhaustively testable (every sequence can be fed
// whole and byte-by-byte and must decode identically) and leaves policy with
// the caller. The one inherent ambiguity in terminal input, a lone Escape
// press versus the first byte of an escape sequence, is resolved by the
// caller's read loop: when a read returns and the decoder reports a pending
// partial sequence, the caller waits briefly for more bytes and calls Flush
// if none arrive, which resolves the pending Escape as the Escape key.
//
// The decoder understands the kitty keyboard protocol (CSI u) and falls back
// to the legacy encodings (CSI and SS3 function keys, modifier parameters,
// control bytes, Alt as an Escape prefix), so one event vocabulary covers
// both modern terminals and every ConPTY, tmux, and legacy emulator behind
// the same interface.
package input

// Event is one decoded input event: a Key, a Paste, a Focus change, or an
// Unknown sequence.
type Event interface{ isEvent() }

// Mod is a bitmask of key modifiers.
type Mod uint8

// The modifier bits, matching the bitmask the kitty and xterm encodings
// share on the wire.
const (
	ModShift Mod = 1 << iota
	ModAlt
	ModCtrl
	ModSuper
)

// Key is one key press. Printable keys carry their rune in Code; functional
// keys use the Key* constants (private-use runes, so they can never collide
// with real text).
type Key struct {
	Code rune
	Mods Mod
}

func (Key) isEvent() {}

// Text returns the text this key inserts, or the empty string for a
// functional key or a modified key.
func (k Key) Text() string {
	if k.IsFunctional() || k.Mods&(ModCtrl|ModAlt|ModSuper) != 0 || k.Code < ' ' {
		return ""
	}
	return string(k.Code)
}

// IsFunctional reports whether the key is one of the Key* functional keys
// rather than text. The check is an exact range: code points above the
// functional block (emoji, supplementary-plane text) are ordinary text.
func (k Key) IsFunctional() bool {
	return k.Code >= keyBase && k.Code < keyLimit
}

// Paste is one complete bracketed paste, however many reads it arrived in.
// Line endings are normalized to \n.
type Paste struct{ Text string }

func (Paste) isEvent() {}

// Focus reports the terminal gaining or losing focus (mode 1004 events).
type Focus struct{ Gained bool }

func (Focus) isEvent() {}

// Unknown carries an escape sequence the decoder does not map. It is
// surfaced rather than dropped so a key diagnostic view can show exactly
// what the terminal sent.
type Unknown struct{ Seq string }

func (Unknown) isEvent() {}

// Functional key codes, in the Unicode private use area.
const keyBase rune = 0xE000

// The functional keys every supported encoding (kitty, legacy CSI, SS3,
// control bytes) normalizes onto.
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

	// keyLimit marks the end of the functional block; it must stay last.
	keyLimit
)
