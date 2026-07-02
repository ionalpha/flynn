package mdstream_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/mdstream"
)

// FuzzSplitter streams arbitrary text through the splitter in arbitrary
// chunk sizes and asserts the invariants a renderer depends on: no panic,
// the committed prefix plus the tail reconstructs the input byte for byte,
// and Close flushes the remainder exactly once. The splitter consumes model
// output, which can contain any bytes at all, including pathological
// almost-markdown.
func FuzzSplitter(f *testing.F) {
	f.Add("plain paragraph\n\nnext", 3)
	f.Add("```go\ncode\n```\nafter", 1)
	f.Add("~~~\n```\n~~~\n", 2)
	f.Add("| a |\n|---|\n| 1 |\n\n", 5)
	f.Add("````\n``` inner\n````\n", 4)
	f.Add("\x00\xff\r\n\r`~ ", 1)

	f.Fuzz(func(t *testing.T, text string, chunk int) {
		if chunk < 1 {
			chunk = 1
		}
		var s mdstream.Splitter
		var stable strings.Builder
		for i := 0; i < len(text); i += chunk {
			end := min(i+chunk, len(text))
			s.Write(text[i:end])
			stable.WriteString(s.Stable())
		}
		if got := stable.String() + s.Tail(); got != text {
			t.Fatalf("reconstruction mismatch:\n got %q\nwant %q", got, text)
		}
		rest := s.Close()
		if got := stable.String() + rest; got != text {
			t.Fatalf("close mismatch:\n got %q\nwant %q", got, text)
		}
		if s.Tail() != "" {
			t.Fatalf("tail after Close = %q", s.Tail())
		}
	})
}
