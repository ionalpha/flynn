package main

import (
	"strconv"
	"testing"

	"pgregory.net/rapid"
)

// TestParseChoice covers the menu parser's table of cases: new-session sentinels,
// valid selections, and the out-of-range and junk entries that must fall back to a
// new session rather than select a run that is not listed.
func TestParseChoice(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want int
	}{
		{"", 5, 0},
		{"  ", 5, 0},
		{"0", 5, 0},
		{"1", 5, 1},
		{"5", 5, 5},
		{" 3 ", 5, 3},
		{"6", 5, 0},     // past the end
		{"-1", 5, 0},    // negative
		{"x", 5, 0},     // not a number
		{"1.5", 5, 0},   // not an integer
		{"99", 5, 0},    // way past the end
		{"1", 0, 0},     // empty menu: nothing is selectable
		{"01", 5, 1},    // leading zero still parses to 1
		{"2abc", 5, 0},  // trailing junk
		{"  7  ", 9, 7}, // surrounding whitespace
	}
	for _, c := range cases {
		if got := parseChoice(c.in, c.n); got != c.want {
			t.Errorf("parseChoice(%q, %d) = %d, want %d", c.in, c.n, got, c.want)
		}
	}
}

// TestProp_ParseChoiceInRange: whatever the input and menu length, the parser
// returns either 0 (new session) or a 1-based index that is genuinely on the menu.
// It can never point past the list, so the caller's goals[n-1] index is always safe.
func TestProp_ParseChoiceInRange(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 50).Draw(rt, "n")
		in := rapid.String().Draw(rt, "in")
		got := parseChoice(in, n)
		if got < 0 || got > n {
			rt.Fatalf("parseChoice(%q, %d) = %d, out of [0,%d]", in, n, got, n)
		}
	})
}

// TestProp_ParseChoiceRoundTrips: a valid 1-based number renders and parses back to
// itself, so the menu's printed indices always select the run they label.
func TestProp_ParseChoiceRoundTrips(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 50).Draw(rt, "n")
		want := rapid.IntRange(1, n).Draw(rt, "want")
		if got := parseChoice(strconv.Itoa(want), n); got != want {
			rt.Fatalf("parseChoice(%d, %d) = %d, want %d", want, n, got, want)
		}
	})
}

// FuzzParseChoice throws arbitrary entries and menu lengths at the parser, which
// reads a value the user types at the prompt. The invariant: it never panics and
// never returns an index outside [0, n], so a menu choice can never index past the
// session list however hostile the bytes.
func FuzzParseChoice(f *testing.F) {
	for _, s := range []string{"", "0", "1", "-1", "x", "1.5", "999999999999999999999", "\x00", " 2 "} {
		f.Add(s, 5)
	}
	f.Fuzz(func(t *testing.T, in string, n int) {
		if n < 0 || n > 1_000_000 {
			return // a menu is never this large; keep the bound meaningful
		}
		got := parseChoice(in, n)
		if got < 0 || got > n {
			t.Fatalf("parseChoice(%q, %d) = %d, out of [0,%d]", in, n, got, n)
		}
	})
}
