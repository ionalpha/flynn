package mdstream_test

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/tui/mdstream"
)

// genMarkdown draws markdown-shaped text from the constructs the splitter
// makes decisions about: paragraphs, blank lines, fences of varying markers
// and lengths, table rows, and unterminated trailing lines.
func genMarkdown(rt *rapid.T) string {
	var b strings.Builder
	blocks := rapid.IntRange(0, 8).Draw(rt, "blocks")
	for range blocks {
		switch rapid.IntRange(0, 4).Draw(rt, "block") {
		case 0:
			b.WriteString(rapid.StringMatching(`[a-z ]{0,20}`).Draw(rt, "para"))
			b.WriteString("\n")
		case 1:
			b.WriteString("\n")
		case 2:
			marker := rapid.SampledFrom([]string{"```", "````", "~~~"}).Draw(rt, "marker")
			b.WriteString(marker + "\n")
			b.WriteString(rapid.StringMatching(`[a-z\n ]{0,30}`).Draw(rt, "code"))
			if rapid.Bool().Draw(rt, "closed") {
				b.WriteString("\n" + marker + "\n")
			}
		case 3:
			b.WriteString("| a | b |\n|---|---|\n| 1 | 2 |\n")
		case 4:
			b.WriteString(rapid.StringMatching(`[a-z`+"`"+`~ ]{0,15}`).Draw(rt, "partial"))
		}
	}
	return b.String()
}

// chunked streams text through a splitter in the given chunk sizes,
// collecting every Stable delta, and returns (committed, tail).
func chunked(text string, sizes func(remaining int) int) (string, string) {
	var s mdstream.Splitter
	var stable strings.Builder
	for len(text) > 0 {
		n := sizes(len(text))
		s.Write(text[:n])
		stable.WriteString(s.Stable())
		text = text[n:]
	}
	return stable.String(), s.Tail()
}

// TestProp_SplitterReconstructsTheStream: however the text is chunked, the
// committed prefix plus the tail is byte-for-byte the input. Rendering
// stable and tail separately can therefore never lose or duplicate text.
func TestProp_SplitterReconstructsTheStream(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := genMarkdown(rt)
		if text == "" {
			return
		}
		maxChunk := rapid.IntRange(1, 7).Draw(rt, "chunk")
		stable, tail := chunked(text, func(remaining int) int {
			return min(maxChunk, remaining)
		})
		if stable+tail != text {
			rt.Fatalf("reconstruction lost bytes:\nstable %q\ntail %q\ninput %q", stable, tail, text)
		}
	})
}

// TestProp_SplitterIsChunkInvariant: the final (stable, tail) partition is a
// function of the text alone, never of how the network happened to chunk it.
func TestProp_SplitterIsChunkInvariant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := genMarkdown(rt)
		if text == "" {
			return
		}
		wantStable, wantTail := chunked(text, func(remaining int) int { return remaining })
		chunk := rapid.IntRange(1, 5).Draw(rt, "chunk")
		stable, tail := chunked(text, func(remaining int) int { return min(chunk, remaining) })
		if stable != wantStable || tail != wantTail {
			rt.Fatalf("chunking changed the partition:\n got %q / %q\nwant %q / %q", stable, tail, wantStable, wantTail)
		}
	})
}

// TestProp_CloseFlushesEverything: after Close, every byte has been
// delivered exactly once and the tail is empty.
func TestProp_CloseFlushesEverything(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := genMarkdown(rt)
		var s mdstream.Splitter
		var out strings.Builder
		for i := 0; i < len(text); i += 3 {
			s.Write(text[i:min(i+3, len(text))])
			out.WriteString(s.Stable())
		}
		out.WriteString(s.Close())
		if out.String() != text {
			rt.Fatalf("close lost bytes:\n got %q\nwant %q", out.String(), text)
		}
		if s.Tail() != "" {
			rt.Fatalf("tail after Close = %q", s.Tail())
		}
	})
}

// TestProp_BoundariesLandOnLineEnds: the committed prefix always ends at a
// newline (or is empty), so a renderer never receives half a line as stable.
func TestProp_BoundariesLandOnLineEnds(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := genMarkdown(rt)
		var s mdstream.Splitter
		for i := 0; i < len(text); i += 2 {
			s.Write(text[i:min(i+2, len(text))])
			if stable := s.Stable(); stable != "" && !strings.HasSuffix(stable, "\n") {
				rt.Fatalf("stable delta %q does not end at a line end", stable)
			}
		}
	})
}
