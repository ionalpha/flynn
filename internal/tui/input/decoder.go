package input

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxSeqLen bounds how many bytes one escape sequence may span. A stream that
// opens a sequence and never finishes it (line noise, a binary paste outside
// paste mode) is surfaced as Unknown and dropped once it exceeds the bound,
// so garbage can never wedge the decoder waiting forever.
const maxSeqLen = 64

// Decoder turns raw terminal bytes into events. Feed as many or as few bytes
// as each read produced; partial sequences are buffered and complete on a
// later Feed. The zero value is ready to use.
type Decoder struct {
	buf     []byte
	inPaste bool
	paste   []byte
}

// Pending reports whether the decoder is holding an incomplete sequence. The
// read loop uses it to decide whether a short idle wait plus Flush is needed
// to resolve a lone Escape.
func (d *Decoder) Pending() bool { return len(d.buf) > 0 || d.inPaste }

// MidPaste reports whether a bracketed paste is still streaming in. The read
// loop must not Flush on idle while it is true: a large paste can stall
// between chunks, and flushing would split it into two events.
func (d *Decoder) MidPaste() bool { return d.inPaste }

// Feed consumes the next chunk of the input stream and returns the events it
// completes. Bytes that end mid-sequence stay buffered for the next Feed.
func (d *Decoder) Feed(p []byte) []Event {
	d.buf = append(d.buf, p...)
	var evs []Event
	for {
		ev, n, ok := d.next()
		if !ok {
			return evs
		}
		d.buf = d.buf[n:]
		if ev != nil {
			evs = append(evs, ev)
		}
	}
}

// Flush resolves whatever is buffered as final: a pending lone Escape becomes
// the Escape key, an incomplete sequence is surfaced as Unknown, an
// unterminated paste is delivered with what arrived, and an incomplete
// multi-byte character is dropped. The caller invokes it when the stream
// ends, or after a short idle wait while Pending, which is what turns the
// Escape-versus-sequence ambiguity into a decision.
func (d *Decoder) Flush() []Event {
	var evs []Event
	if d.inPaste {
		evs = append(evs, Paste{Text: normalizePaste(d.paste)})
		d.paste = nil
		d.inPaste = false
	}
	for len(d.buf) > 0 {
		ev, n, ok := d.next()
		if ok {
			d.buf = d.buf[n:]
			if ev != nil {
				evs = append(evs, ev)
			}
			continue
		}
		// Whatever remains is incomplete and no more bytes are coming.
		if d.buf[0] == 0x1b {
			if len(d.buf) == 1 {
				evs = append(evs, Key{Code: KeyEsc})
			} else {
				evs = append(evs, Unknown{Seq: string(d.buf)})
			}
		}
		// An incomplete multi-byte character (or the Unknown above) is consumed.
		d.buf = nil
	}
	return evs
}

// next decodes one event from the front of the buffer. It returns ok=false
// when the buffer is empty or holds only an incomplete sequence, ev=nil with
// ok=true for bytes that consume without producing an event (a paste chunk,
// a swallowed release event).
func (d *Decoder) next() (ev Event, n int, ok bool) {
	if d.inPaste {
		return d.pasteChunk()
	}
	if len(d.buf) == 0 {
		return nil, 0, false
	}
	b := d.buf[0]
	if b == 0x1b {
		return d.escape()
	}
	if b < 0x20 || b == 0x7f {
		return controlKey(b, 0), 1, true
	}
	if !utf8.FullRune(d.buf) {
		if len(d.buf) >= utf8.UTFMax {
			return nil, 1, true // invalid leading byte: drop it and keep moving
		}
		return nil, 0, false
	}
	r, size := utf8.DecodeRune(d.buf)
	if r == utf8.RuneError && size == 1 {
		return nil, 1, true // invalid byte: drop
	}
	return Key{Code: r}, size, true
}

// pasteChunk consumes buffered bytes while inside a bracketed paste, ending
// the paste when the terminator arrives. Bytes that could begin the
// terminator stay buffered so a terminator split across reads still matches.
func (d *Decoder) pasteChunk() (Event, int, bool) {
	if len(d.buf) == 0 {
		return nil, 0, false
	}
	if i := bytes.Index(d.buf, []byte(pasteEnd)); i >= 0 {
		d.paste = append(d.paste, d.buf[:i]...)
		text := normalizePaste(d.paste)
		d.paste = nil
		d.inPaste = false
		return Paste{Text: text}, i + len(pasteEnd), true
	}
	// Keep any tail that is a prefix of the terminator; consume the rest.
	keep := len(d.buf)
	for k := 1; k < len(pasteEnd) && k <= len(d.buf); k++ {
		if bytes.HasPrefix([]byte(pasteEnd), d.buf[len(d.buf)-k:]) {
			keep = len(d.buf) - k
		}
	}
	if keep == 0 {
		return nil, 0, false
	}
	d.paste = append(d.paste, d.buf[:keep]...)
	return nil, keep, true
}

// escape decodes a sequence starting with ESC at the front of the buffer.
func (d *Decoder) escape() (Event, int, bool) {
	if len(d.buf) == 1 {
		return nil, 0, false // a lone ESC so far; Feed may complete it, Flush resolves it
	}
	switch d.buf[1] {
	case '[':
		return d.csi()
	case 'O':
		if len(d.buf) < 3 {
			return nil, 0, false
		}
		return ss3Key(d.buf[2]), 3, true
	case 0x1b:
		return Key{Code: KeyEsc, Mods: ModAlt}, 2, true
	}
	// Alt as an ESC prefix: the next control byte or character carries ModAlt.
	rest := d.buf[1:]
	b := rest[0]
	if b < 0x20 || b == 0x7f {
		return controlKey(b, ModAlt), 2, true
	}
	if !utf8.FullRune(rest) {
		if len(rest) >= utf8.UTFMax {
			return nil, 2, true
		}
		return nil, 0, false
	}
	r, size := utf8.DecodeRune(rest)
	if r == utf8.RuneError && size == 1 {
		return nil, 2, true
	}
	return Key{Code: r, Mods: ModAlt}, 1 + size, true
}

// csi decodes a CSI sequence (ESC [ parameters final-byte).
func (d *Decoder) csi() (Event, int, bool) {
	// Find the final byte (0x40..0x7e) after the parameter and intermediate
	// bytes (0x20..0x3f).
	i := 2
	for ; i < len(d.buf); i++ {
		if c := d.buf[i]; c >= 0x40 && c <= 0x7e {
			break
		}
		if c := d.buf[i]; c < 0x20 || c > 0x3f {
			// Not a CSI byte at all: the sequence is malformed. Surface it.
			return Unknown{Seq: string(d.buf[:i+1])}, i + 1, true
		}
	}
	if i == len(d.buf) {
		if len(d.buf) > maxSeqLen {
			return Unknown{Seq: string(d.buf)}, len(d.buf), true
		}
		return nil, 0, false
	}
	params, final := string(d.buf[2:i]), d.buf[i]
	n := i + 1
	if params == "200" && final == '~' {
		d.inPaste = true
		return nil, n, true
	}
	return csiKey(params, final, string(d.buf[:n])), n, true
}

// csiKey maps a complete CSI sequence to its event.
func csiKey(params string, final byte, raw string) Event {
	switch final {
	case 'u':
		return kittyKey(params, raw)
	case 'A', 'B', 'C', 'D', 'H', 'F':
		code := map[byte]rune{
			'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft,
			'H': KeyHome, 'F': KeyEnd,
		}[final]
		return Key{Code: code, Mods: paramMods(params)}
	case 'Z':
		return Key{Code: KeyTab, Mods: ModShift}
	case 'I':
		return Focus{Gained: true}
	case 'O':
		return Focus{Gained: false}
	case '~':
		return tildeKey(params, raw)
	}
	return Unknown{Seq: raw}
}

// tildeKey maps the legacy CSI number ~ function keys.
func tildeKey(params, raw string) Event {
	fields := strings.Split(params, ";")
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return Unknown{Seq: raw}
	}
	var mods Mod
	if len(fields) > 1 {
		mods = paramMods(params)
	}
	keys := map[int]rune{
		1: KeyHome, 2: KeyInsert, 3: KeyDelete, 4: KeyEnd,
		5: KeyPageUp, 6: KeyPageDown, 7: KeyHome, 8: KeyEnd,
		11: KeyF1, 12: KeyF2, 13: KeyF3, 14: KeyF4, 15: KeyF5,
		17: KeyF6, 18: KeyF7, 19: KeyF8, 20: KeyF9, 21: KeyF10,
		23: KeyF11, 24: KeyF12,
	}
	k, found := keys[code]
	if !found {
		return Unknown{Seq: raw}
	}
	return Key{Code: k, Mods: mods}
}

// kittyKey decodes a kitty keyboard protocol key (CSI unicode;mods u). The
// unicode field may carry alternate layouts after a colon and the mods field
// an event type; only the primary values matter here, and release events are
// swallowed so a press-only consumer sees each key once.
func kittyKey(params, raw string) Event {
	fields := strings.Split(params, ";")
	codeStr, _, _ := strings.Cut(fields[0], ":")
	code, err := strconv.Atoi(codeStr)
	if err != nil || code < 0 || code > utf8.MaxRune || !utf8.ValidRune(rune(code)) { //nolint:gosec // G115: bounded to [0, MaxRune] on this line
		return Unknown{Seq: raw}
	}
	mods, event := 1, 1
	if len(fields) > 1 {
		modStr, evStr, hasEvent := strings.Cut(fields[1], ":")
		if v, err := strconv.Atoi(modStr); err == nil {
			mods = v
		}
		if hasEvent {
			if v, err := strconv.Atoi(evStr); err == nil {
				event = v
			}
		}
	}
	if event == 3 {
		return nil // key release
	}
	k := Key{Code: rune(code), Mods: modBits(mods)}
	switch code {
	case 13:
		k.Code = KeyEnter
	case 27:
		k.Code = KeyEsc
	case 9:
		k.Code = KeyTab
	case 127:
		k.Code = KeyBackspace
	}
	return k
}

// ss3Key maps the SS3 (ESC O) function keys sent in application mode.
func ss3Key(final byte) Event {
	switch final {
	case 'A':
		return Key{Code: KeyUp}
	case 'B':
		return Key{Code: KeyDown}
	case 'C':
		return Key{Code: KeyRight}
	case 'D':
		return Key{Code: KeyLeft}
	case 'H':
		return Key{Code: KeyHome}
	case 'F':
		return Key{Code: KeyEnd}
	case 'P':
		return Key{Code: KeyF1}
	case 'Q':
		return Key{Code: KeyF2}
	case 'R':
		return Key{Code: KeyF3}
	case 'S':
		return Key{Code: KeyF4}
	}
	return Unknown{Seq: "\x1bO" + string(final)}
}

// controlKey maps a raw control byte to its key. The Enter, Tab, and
// Backspace bytes map to their functional keys; Ctrl+J stays Ctrl+J rather
// than folding into Enter, because a composer binds it separately (insert a
// newline versus submit).
func controlKey(b byte, mods Mod) Event {
	switch b {
	case 0x0d:
		return Key{Code: KeyEnter, Mods: mods}
	case 0x09:
		return Key{Code: KeyTab, Mods: mods}
	case 0x7f:
		return Key{Code: KeyBackspace, Mods: mods}
	case 0x08:
		return Key{Code: KeyBackspace, Mods: mods | ModCtrl}
	case 0x00:
		return Key{Code: ' ', Mods: mods | ModCtrl}
	case 0x1b:
		return Key{Code: KeyEsc, Mods: mods}
	}
	switch {
	case b >= 0x01 && b <= 0x1a:
		return Key{Code: 'a' + rune(b) - 1, Mods: mods | ModCtrl}
	case b >= 0x1c && b <= 0x1f:
		return Key{Code: '\\' + rune(b) - 0x1c, Mods: mods | ModCtrl}
	}
	return Unknown{Seq: string(b)}
}

// paramMods extracts the xterm modifier parameter (the value after the first
// semicolon, encoded as one plus the modifier bitmask).
func paramMods(params string) Mod {
	_, modStr, found := strings.Cut(params, ";")
	if !found {
		return 0
	}
	modStr, _, _ = strings.Cut(modStr, ":")
	v, err := strconv.Atoi(modStr)
	if err != nil {
		return 0
	}
	return modBits(v)
}

// modBits converts the wire modifier value (one plus the bitmask shared by
// the xterm and kitty encodings) into a Mod.
func modBits(v int) Mod {
	if v < 2 {
		return 0
	}
	bits := v - 1
	var m Mod
	if bits&1 != 0 {
		m |= ModShift
	}
	if bits&2 != 0 {
		m |= ModAlt
	}
	if bits&4 != 0 {
		m |= ModCtrl
	}
	if bits&8 != 0 {
		m |= ModSuper
	}
	return m
}

// normalizePaste converts the paste's line endings to \n, covering both CRLF
// and the bare CR bursts the Windows console delivers.
func normalizePaste(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

const pasteEnd = "\x1b[201~"
