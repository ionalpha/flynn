package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/session"
)

// TestRenderTokensBreakdown proves the breakdown names the total input, the cached
// portion with its share of input, cache writes, and output, plus the note that
// explains why input is large.
func TestRenderTokensBreakdown(t *testing.T) {
	var buf bytes.Buffer
	renderTokens(&buf, session.Usage{InputTokens: 71700, OutputTokens: 392, CacheReadTokens: 68000, CacheWriteTokens: 2100}, 10)
	out := buf.String()
	for _, want := range []string{"tokens this run (10 turns)", "71.7k", "from cache", "68.0k", "94%", "output", "392", "cache write", "served from cache"} {
		if !strings.Contains(out, want) {
			t.Errorf("token breakdown missing %q:\n%s", want, out)
		}
	}
}

// TestRenderTokensEmpty proves a run with no usage reports so rather than a table of
// zeros.
func TestRenderTokensEmpty(t *testing.T) {
	var buf bytes.Buffer
	renderTokens(&buf, session.Usage{}, 0)
	if !strings.Contains(buf.String(), "no tokens used yet") {
		t.Errorf("empty usage should report none: %q", buf.String())
	}
}
