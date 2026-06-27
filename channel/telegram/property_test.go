package telegram

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestSplitMessageProperty is the rigor property: for any text and any positive
// limit, the chunks rejoin to exactly the original (nothing is lost, added, or
// reordered), every chunk is within the limit, and no empty chunk is produced. So
// a reply of any length and any unicode content is delivered faithfully in pieces.
func TestSplitMessageProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.String().Draw(rt, "s")
		limit := rapid.IntRange(1, 50).Draw(rt, "limit")

		parts := splitMessage(s, limit)

		if strings.Join(parts, "") != s {
			rt.Fatalf("rejoined chunks != original")
		}
		if s == "" && len(parts) != 0 {
			rt.Fatalf("empty input produced %d chunks, want 0", len(parts))
		}
		for i, p := range parts {
			n := len([]rune(p))
			if n == 0 {
				rt.Fatalf("chunk %d is empty", i)
			}
			if n > limit {
				rt.Fatalf("chunk %d has %d runes, exceeds limit %d", i, n, limit)
			}
		}
	})
}
