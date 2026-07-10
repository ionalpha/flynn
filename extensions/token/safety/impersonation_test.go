package safety

import "testing"

func TestImpersonationTarget(t *testing.T) {
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
	}
	for _, c := range cases {
		if got := ImpersonationTarget(c.name, c.symbol); got != c.want {
			t.Errorf("ImpersonationTarget(%q, %q) = %q, want %q", c.name, c.symbol, got, c.want)
		}
	}
}
