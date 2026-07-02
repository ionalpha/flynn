package mdstream_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/mdstream"
)

// feed streams text into a fresh splitter in chunks of the given size and
// returns everything committed plus the final tail.
func feed(text string, chunk int) (stable, tail string) {
	var s mdstream.Splitter
	var out strings.Builder
	for i := 0; i < len(text); i += chunk {
		end := i + chunk
		if end > len(text) {
			end = len(text)
		}
		s.Write(text[i:end])
		out.WriteString(s.Stable())
	}
	return out.String(), s.Tail()
}

func TestBlankLineCommitsTheBlock(t *testing.T) {
	stable, tail := feed("first paragraph\n\nsecond still stre", 1)
	if stable != "first paragraph\n\n" {
		t.Fatalf("stable = %q, want the completed paragraph", stable)
	}
	if tail != "second still stre" {
		t.Fatalf("tail = %q, want the in-flight paragraph", tail)
	}
}

func TestOpenFenceHoldsEverything(t *testing.T) {
	text := "intro\n\n```go\ncode line\n\nstill code after a blank\n"
	stable, tail := feed(text, 3)
	if stable != "intro\n\n" {
		t.Fatalf("stable = %q, want only the intro", stable)
	}
	if !strings.Contains(tail, "still code after a blank") {
		t.Fatalf("tail = %q, want the whole open fence held back", tail)
	}
}

func TestClosedFenceIsABoundary(t *testing.T) {
	text := "```\ncode\n```\nafter"
	stable, tail := feed(text, 1)
	if stable != "```\ncode\n```\n" {
		t.Fatalf("stable = %q, want the closed fence committed without a blank line", stable)
	}
	if tail != "after" {
		t.Fatalf("tail = %q", tail)
	}
}

func TestFenceCloseNeedsTheOpeningLength(t *testing.T) {
	text := "````\n``` not a close\n````\n"
	stable, _ := feed(text, 1)
	if stable != text {
		t.Fatalf("stable = %q, want the four-backtick fence closed only by four backticks", stable)
	}
}

func TestTildeAndBacktickFencesAreIndependent(t *testing.T) {
	text := "~~~\n``` inside a tilde fence\n~~~\n"
	stable, _ := feed(text, 2)
	if stable != text {
		t.Fatalf("stable = %q, want backticks inside a tilde fence to not open a fence", stable)
	}
}

func TestBacktickInfoStringWithBacktickIsNotAFence(t *testing.T) {
	// "``` `x` ```" is an inline code span line, not a fence opener.
	text := "``` `x` ```\n\nnext\n\n"
	stable, _ := feed(text, 1)
	if stable != text {
		t.Fatalf("stable = %q, want the inline-span line to not open a fence", stable)
	}
}

func TestStreamingTableStaysInTheTail(t *testing.T) {
	text := "before\n\n| a | b |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |\n"
	stable, tail := feed(text, 4)
	if stable != "before\n\n" {
		t.Fatalf("stable = %q, want the table held back while it streams", stable)
	}
	if !strings.HasPrefix(tail, "| a | b |") {
		t.Fatalf("tail = %q, want the whole table", tail)
	}
}

func TestCloseFlushesTheTail(t *testing.T) {
	var s mdstream.Splitter
	s.Write("open fence\n```\nnever closed\n")
	_ = s.Stable()
	rest := s.Close()
	if !strings.Contains(rest, "never closed") {
		t.Fatalf("Close = %q, want the held-back tail flushed", rest)
	}
	if s.Tail() != "" {
		t.Fatalf("tail after Close = %q, want empty", s.Tail())
	}
}

func TestChunkInvariance(t *testing.T) {
	text := "para one\n\n```go\nfunc x() {}\n```\n\n| a |\n|---|\n| 1 |\n\ntail text"
	wantStable, wantTail := feed(text, len(text))
	for chunk := 1; chunk <= 7; chunk++ {
		stable, tail := feed(text, chunk)
		if stable != wantStable || tail != wantTail {
			t.Fatalf("chunk %d: stable %q tail %q, want %q / %q", chunk, stable, tail, wantStable, wantTail)
		}
	}
}

func TestIncompleteLineNeverCommits(t *testing.T) {
	var s mdstream.Splitter
	s.Write("text\n\npartial line without newline")
	if got := s.Stable(); got != "text\n\n" {
		t.Fatalf("stable = %q", got)
	}
	if got := s.Tail(); got != "partial line without newline" {
		t.Fatalf("tail = %q, want the unterminated line held", got)
	}
	// The partial line later turns out to be a fence opener: committing it
	// early would have been wrong.
	s.Write("\n``` ignored\n")
	_ = s.Stable()
	if !strings.Contains(s.Tail(), "``` ignored") {
		t.Fatalf("tail = %q, want the fence content held", s.Tail())
	}
}

func TestIndentedFenceMarkersAreCode(t *testing.T) {
	// Four spaces of indentation makes an indented code block line, not a
	// fence; the blank line after it still commits.
	text := "    ```\n\nplain\n\n"
	stable, _ := feed(text, 1)
	if stable != text {
		t.Fatalf("stable = %q, want indented markers treated as code, not a fence", stable)
	}
}
