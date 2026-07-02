package input_test

import (
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/input"
)

// genToken draws one well-formed input token: a printable string, a control
// byte, a known escape sequence, an Alt-prefixed key, or a bounded paste.
// Tokens are well-formed on purpose: the chunk-invariance property is stated
// over real terminal traffic, where the one deliberate exception (the
// runaway-sequence cap, which by design depends on how many bytes have
// arrived) does not fire.
func genToken(rt *rapid.T) string {
	switch rapid.IntRange(0, 5).Draw(rt, "kind") {
	case 0:
		return rapid.StringMatching(`[ -~é日🙂]{1,6}`).Draw(rt, "text")
	case 1:
		return string(rune(rapid.IntRange(1, 26).Draw(rt, "ctrl")))
	case 2:
		return rapid.SampledFrom([]string{
			"\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", "\x1b[H", "\x1b[F", "\x1b[Z",
			"\x1b[1;5C", "\x1b[3~", "\x1b[5~", "\x1b[24~", "\x1b[3;2~",
			"\x1bOP", "\x1bOS", "\x1bOA",
			"\x1b[97u", "\x1b[97;6u", "\x1b[13u", "\x1b[97;1:3u",
			"\x1b[I", "\x1b[O",
		}).Draw(rt, "seq")
	case 3:
		return "\x1b" + rapid.StringMatching(`[a-z]`).Draw(rt, "alt")
	case 4:
		return "\x1b[200~" + rapid.StringMatching(`[ -~\n]{0,12}`).Draw(rt, "paste") + "\x1b[201~"
	default:
		return "\x1b\x1b"
	}
}

func genStream(rt *rapid.T) string {
	n := rapid.IntRange(0, 8).Draw(rt, "tokens")
	var s strings.Builder
	for range n {
		s.WriteString(genToken(rt))
	}
	return s.String()
}

func decodeWhole(stream string) []input.Event {
	var d input.Decoder
	evs := d.Feed([]byte(stream))
	return append(evs, d.Flush()...)
}

// TestProp_DecoderIsChunkInvariant is the guarantee the decoder exists for:
// however the terminal's bytes are split across reads, the decoded events
// are identical. Every split point of every generated stream must agree
// with the whole-stream decode.
func TestProp_DecoderIsChunkInvariant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		stream := genStream(rt)
		if stream == "" {
			return
		}
		want := decodeWhole(stream)
		cut := rapid.IntRange(1, max(1, len(stream)-1)).Draw(rt, "cut")
		var d input.Decoder
		evs := d.Feed([]byte(stream[:cut]))
		evs = append(evs, d.Feed([]byte(stream[cut:]))...)
		evs = append(evs, d.Flush()...)
		if !reflect.DeepEqual(evs, want) {
			rt.Fatalf("split at %d changed the events:\n got %#v\nwant %#v", cut, evs, want)
		}
	})
}

// TestProp_DecoderNeverWedgesOnArbitraryBytes: any byte stream at all (line
// noise, binary garbage, truncated sequences) decodes without panicking, and
// Flush always drains the decoder completely, so no input can leave the
// session's read loop stuck holding bytes forever.
func TestProp_DecoderNeverWedgesOnArbitraryBytes(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		raw := rapid.SliceOfN(rapid.Byte(), 0, 64).Draw(rt, "raw")
		var d input.Decoder
		d.Feed(raw)
		d.Flush()
		if d.Pending() {
			rt.Fatalf("decoder still pending after Flush on %q", raw)
		}
	})
}

// TestProp_PrintableTextRoundTrips: a stream of plain printable text decodes
// to keys whose Text() concatenation is the input, so typing is lossless.
func TestProp_PrintableTextRoundTrips(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := rapid.StringMatching(`[ -~é日🙂]{0,16}`).Draw(rt, "text")
		var got strings.Builder
		for _, ev := range decodeWhole(text) {
			k, isKey := ev.(input.Key)
			if !isKey {
				rt.Fatalf("plain text decoded a non-key event %#v", ev)
			}
			got.WriteString(k.Text())
		}
		if got.String() != text {
			rt.Fatalf("text round-trip lost bytes: got %q want %q", got.String(), text)
		}
	})
}

// TestProp_PasteContentIsOpaque: whatever printable content a paste carries
// (including escape-sequence look-alikes), it arrives as exactly one Paste
// event carrying that content, never as keys.
func TestProp_PasteContentIsOpaque(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		content := rapid.StringMatching(`[ -~\né日]{0,20}`).Draw(rt, "content")
		evs := decodeWhole("\x1b[200~" + content + "\x1b[201~")
		if len(evs) != 1 {
			rt.Fatalf("paste decoded as %d events: %#v", len(evs), evs)
		}
		p, isPaste := evs[0].(input.Paste)
		if !isPaste {
			rt.Fatalf("paste decoded as %#v", evs[0])
		}
		if p.Text != content {
			rt.Fatalf("paste content = %q, want %q", p.Text, content)
		}
	})
}
