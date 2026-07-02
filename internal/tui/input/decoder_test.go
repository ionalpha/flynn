package input_test

import (
	"reflect"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/input"
)

// decode feeds the whole stream and flushes, returning every event.
func decode(t *testing.T, stream string) []input.Event {
	t.Helper()
	var d input.Decoder
	evs := d.Feed([]byte(stream))
	return append(evs, d.Flush()...)
}

// decodeByByte feeds the stream one byte at a time and flushes at the end.
func decodeByByte(t *testing.T, stream string) []input.Event {
	t.Helper()
	var d input.Decoder
	var evs []input.Event
	for i := range len(stream) {
		evs = append(evs, d.Feed([]byte{stream[i]})...)
	}
	return append(evs, d.Flush()...)
}

var decodeCases = []struct {
	name   string
	stream string
	want   []input.Event
}{
	{"plain text", "hi", []input.Event{
		input.Key{Code: 'h'}, input.Key{Code: 'i'},
	}},
	{"multi-byte text", "é日", []input.Event{
		input.Key{Code: 'é'}, input.Key{Code: '日'},
	}},
	{"enter tab backspace", "\r\t\x7f", []input.Event{
		input.Key{Code: input.KeyEnter}, input.Key{Code: input.KeyTab}, input.Key{Code: input.KeyBackspace},
	}},
	{"ctrl letters", "\x01\x03", []input.Event{
		input.Key{Code: 'a', Mods: input.ModCtrl}, input.Key{Code: 'c', Mods: input.ModCtrl},
	}},
	{"ctrl+j stays distinct from enter", "\n", []input.Event{
		input.Key{Code: 'j', Mods: input.ModCtrl},
	}},
	{"lone escape", "\x1b", []input.Event{
		input.Key{Code: input.KeyEsc},
	}},
	{"alt letter", "\x1bx", []input.Event{
		input.Key{Code: 'x', Mods: input.ModAlt},
	}},
	{"alt enter", "\x1b\r", []input.Event{
		input.Key{Code: input.KeyEnter, Mods: input.ModAlt},
	}},
	{"legacy arrows", "\x1b[A\x1b[D", []input.Event{
		input.Key{Code: input.KeyUp}, input.Key{Code: input.KeyLeft},
	}},
	{"modified arrow", "\x1b[1;5C", []input.Event{
		input.Key{Code: input.KeyRight, Mods: input.ModCtrl},
	}},
	{"shift tab", "\x1b[Z", []input.Event{
		input.Key{Code: input.KeyTab, Mods: input.ModShift},
	}},
	{"home end", "\x1b[H\x1b[F", []input.Event{
		input.Key{Code: input.KeyHome}, input.Key{Code: input.KeyEnd},
	}},
	{"tilde keys", "\x1b[3~\x1b[5~\x1b[24~", []input.Event{
		input.Key{Code: input.KeyDelete}, input.Key{Code: input.KeyPageUp}, input.Key{Code: input.KeyF12},
	}},
	{"tilde key with mods", "\x1b[3;2~", []input.Event{
		input.Key{Code: input.KeyDelete, Mods: input.ModShift},
	}},
	{"ss3 function keys", "\x1bOP\x1bOS", []input.Event{
		input.Key{Code: input.KeyF1}, input.Key{Code: input.KeyF4},
	}},
	{"ss3 application arrows", "\x1bOA", []input.Event{
		input.Key{Code: input.KeyUp},
	}},
	{"kitty plain key", "\x1b[97u", []input.Event{
		input.Key{Code: 'a'},
	}},
	{"kitty ctrl+shift key", "\x1b[97;6u", []input.Event{
		input.Key{Code: 'a', Mods: input.ModShift | input.ModCtrl},
	}},
	{"kitty enter and esc", "\x1b[13u\x1b[27u", []input.Event{
		input.Key{Code: input.KeyEnter}, input.Key{Code: input.KeyEsc},
	}},
	{"kitty release swallowed", "\x1b[97;1:3u", nil},
	{"kitty alternate layouts use the primary code", "\x1b[97:65;2u", []input.Event{
		input.Key{Code: 'a', Mods: input.ModShift},
	}},
	{"focus events", "\x1b[I\x1b[O", []input.Event{
		input.Focus{Gained: true}, input.Focus{Gained: false},
	}},
	{"bracketed paste is one event", "\x1b[200~line1\nline2\x1b[201~", []input.Event{
		input.Paste{Text: "line1\nline2"},
	}},
	{"paste normalizes CRLF and CR", "\x1b[200~a\r\nb\rc\x1b[201~", []input.Event{
		input.Paste{Text: "a\nb\nc"},
	}},
	{"paste containing escape-like bytes", "\x1b[200~\x1b[Anot-a-key\x1b[201~", []input.Event{
		input.Paste{Text: "\x1b[Anot-a-key"},
	}},
	{"text around a paste", "x\x1b[200~mid\x1b[201~y", []input.Event{
		input.Key{Code: 'x'}, input.Paste{Text: "mid"}, input.Key{Code: 'y'},
	}},
	{"alt escape", "\x1b\x1b", []input.Event{
		input.Key{Code: input.KeyEsc, Mods: input.ModAlt},
	}},
	{"unknown csi is surfaced", "\x1b[99Q", []input.Event{
		input.Unknown{Seq: "\x1b[99Q"},
	}},
}

func TestDecode(t *testing.T) {
	for _, tc := range decodeCases {
		t.Run(tc.name, func(t *testing.T) {
			got := decode(t, tc.stream)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decode(%q)\n got %#v\nwant %#v", tc.stream, got, tc.want)
			}
		})
	}
}

// TestDecodeChunkInvariance is the property the whole decoder exists for:
// however the byte stream is split across reads, the events are identical.
func TestDecodeChunkInvariance(t *testing.T) {
	for _, tc := range decodeCases {
		t.Run(tc.name, func(t *testing.T) {
			whole := decode(t, tc.stream)
			byByte := decodeByByte(t, tc.stream)
			if !reflect.DeepEqual(whole, byByte) {
				t.Fatalf("byte-at-a-time decode differs:\nwhole  %#v\nbybyte %#v", whole, byByte)
			}
			// And at every single split point.
			for cut := 1; cut < len(tc.stream); cut++ {
				var d input.Decoder
				evs := d.Feed([]byte(tc.stream[:cut]))
				evs = append(evs, d.Feed([]byte(tc.stream[cut:]))...)
				evs = append(evs, d.Flush()...)
				if !reflect.DeepEqual(evs, whole) {
					t.Fatalf("split at %d differs:\n got %#v\nwant %#v", cut, evs, whole)
				}
			}
		})
	}
}

func TestPendingAndMidPaste(t *testing.T) {
	var d input.Decoder
	if evs := d.Feed([]byte{0x1b}); evs != nil {
		t.Fatalf("lone ESC produced %#v before Flush", evs)
	}
	if !d.Pending() {
		t.Fatal("Pending = false with a buffered ESC")
	}
	if d.MidPaste() {
		t.Fatal("MidPaste = true outside a paste")
	}
	d.Flush()

	d.Feed([]byte("\x1b[200~partial"))
	if !d.MidPaste() {
		t.Fatal("MidPaste = false inside a paste")
	}
	evs := d.Feed([]byte("\x1b[201~"))
	want := []input.Event{input.Paste{Text: "partial"}}
	if !reflect.DeepEqual(evs, want) {
		t.Fatalf("paste completion = %#v, want %#v", evs, want)
	}
}

func TestFlushDeliversAnUnterminatedPaste(t *testing.T) {
	var d input.Decoder
	d.Feed([]byte("\x1b[200~never closed"))
	evs := d.Flush()
	want := []input.Event{input.Paste{Text: "never closed"}}
	if !reflect.DeepEqual(evs, want) {
		t.Fatalf("flush mid-paste = %#v, want %#v", evs, want)
	}
	if d.Pending() {
		t.Fatal("decoder still pending after Flush")
	}
}

func TestRunawaySequenceIsBounded(t *testing.T) {
	var d input.Decoder
	// A CSI that never terminates must eventually surface instead of
	// buffering forever.
	junk := make([]byte, 0, 200)
	junk = append(junk, 0x1b, '[')
	for range 198 {
		junk = append(junk, '1')
	}
	evs := d.Feed(junk)
	if len(evs) == 0 {
		t.Fatal("runaway CSI never surfaced")
	}
	if _, isUnknown := evs[0].(input.Unknown); !isUnknown {
		t.Fatalf("runaway CSI surfaced as %#v, want Unknown", evs[0])
	}
}

func TestKeyText(t *testing.T) {
	cases := []struct {
		k    input.Key
		want string
	}{
		{input.Key{Code: 'a'}, "a"},
		{input.Key{Code: '日'}, "日"},
		{input.Key{Code: 'a', Mods: input.ModShift}, "a"},
		{input.Key{Code: 'a', Mods: input.ModCtrl}, ""},
		{input.Key{Code: input.KeyEnter}, ""},
		{input.Key{Code: input.KeyUp}, ""},
	}
	for _, tc := range cases {
		if got := tc.k.Text(); got != tc.want {
			t.Fatalf("Text(%#v) = %q, want %q", tc.k, got, tc.want)
		}
	}
}
