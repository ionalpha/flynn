package input_test

import (
	"testing"

	"github.com/ionalpha/flynn/internal/tui/input"
)

// FuzzDecoder throws arbitrary byte streams at the decoder, split at an
// arbitrary point, and asserts the invariants that keep the session's read
// loop alive on any input: no panic, Flush always drains completely, and a
// well-formed decode never emits a nil event. The decoder sits directly on
// the terminal byte stream, which a hostile pty, a corrupted multiplexer, or
// plain line noise can fill with anything at all.
func FuzzDecoder(f *testing.F) {
	f.Add([]byte("hello"), 2)
	f.Add([]byte("\x1b[A\x1b[1;5C\x1b[3~"), 4)
	f.Add([]byte("\x1b[200~paste\r\nbody\x1b[201~"), 9)
	f.Add([]byte("\x1b[97;1:3u\x1bOP\x1b\x1b"), 5)
	f.Add([]byte{0x1b, '[', '9', '9', 0xff, 0xfe}, 3)
	f.Add([]byte("é日🙂\x00\x7f"), 1)

	f.Fuzz(func(t *testing.T, data []byte, cut int) {
		var d input.Decoder
		if cut < 0 || cut > len(data) {
			cut = len(data) / 2
		}
		evs := d.Feed(data[:cut])
		evs = append(evs, d.Feed(data[cut:])...)
		evs = append(evs, d.Flush()...)
		if d.Pending() {
			t.Fatalf("decoder still pending after Flush on %q", data)
		}
		for _, ev := range evs {
			if ev == nil {
				t.Fatalf("nil event decoded from %q", data)
			}
		}
	})
}
