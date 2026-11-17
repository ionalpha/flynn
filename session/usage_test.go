package session

import (
	"testing"

	"pgregory.net/rapid"
)

// TestUsageAddKeepsEachCounterInItsOwnField pins that cache reads stay inside input
// and cache writes stay on top, so a replayed run reports the same total as the sum
// of its turns.
func TestUsageAddKeepsEachCounterInItsOwnField(t *testing.T) {
	a := Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, CacheWriteTokens: 7}
	b := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 4}
	got := a.Add(b)
	want := Usage{InputTokens: 110, OutputTokens: 25, CacheReadTokens: 64, CacheWriteTokens: 7}
	if got != want {
		t.Fatalf("Add = %+v, want %+v", got, want)
	}
	if tot := got.Tokens().Total(); tot != 142 {
		t.Fatalf("total = %d, want 142 (input 110 + cache writes 7 + output 25)", tot)
	}
}

func TestUsageAddTotalIsAdditiveProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		mk := func(l string) Usage {
			return Usage{
				InputTokens:      rapid.IntRange(0, 1_000_000).Draw(rt, l+"-in"),
				OutputTokens:     rapid.IntRange(0, 1_000_000).Draw(rt, l+"-out"),
				CacheReadTokens:  rapid.IntRange(0, 1_000_000).Draw(rt, l+"-cr"),
				CacheWriteTokens: rapid.IntRange(0, 1_000_000).Draw(rt, l+"-cw"),
			}
		}
		a, b := mk("a"), mk("b")
		if got, want := a.Add(b).Tokens().Total(), a.Tokens().Total()+b.Tokens().Total(); got != want {
			rt.Fatalf("total of sum = %d, sum of totals = %d", got, want)
		}
	})
}
