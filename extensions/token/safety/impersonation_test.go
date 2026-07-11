package safety

import "testing"

func TestImpersonationTarget(t *testing.T) {
	// zwsp is a zero-width space, used to prove invisible characters cannot smuggle a
	// protected brand past the blocklist.
	const zwsp = "\u200b"
	cases := []struct {
		name, symbol, want string
	}{
		{"USD Coin", "USDC", "USDC"},      // matches on both
		{"Totally Legit", "usdc", "USDC"}, // symbol match, case-insensitive
		{"tether", "XYZ", "USDT"},         // name match
		{" Solana ", "X", "Solana"},       // trims whitespace
		{"Flynn", "FLYNN", ""},            // our own token is not a protected brand
		{"Random Coin", "RND", ""},        // no match
		{"", "", ""},                      // empty

		// Homoglyph and invisible-character spoofs must be caught, not just exact ASCII.
		{"Tethеr", "XYZ", "USDT"},         // Cyrillic 'е' (U+0435) in "Tether"
		{"Fine", "USDТ", "USDT"},          // Cyrillic 'Т' (U+0422) in "USDT"
		{"Sоl", "X", "SOL"},               // Cyrillic 'о' (U+043E) in "SOL"
		{"X", "USD" + zwsp + "C", "USDC"}, // zero-width space inside "USDC"
		{"USD-Coin", "X", "USDC"},         // separator variant of "USD Coin"
		{"u s d c", "X", "USDC"},          // spaced-out symbol

		// Digits carry meaning and must NOT be folded away (no false positives).
		{"ETH2", "ETH2", ""}, // distinct from ETH
		{"USDC1", "X", ""},   // distinct from USDC
	}
	for _, c := range cases {
		if got := ImpersonationTarget(c.name, c.symbol); got != c.want {
			t.Errorf("ImpersonationTarget(%q, %q) = %q, want %q", c.name, c.symbol, got, c.want)
		}
	}
}
