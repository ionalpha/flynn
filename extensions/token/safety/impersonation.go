package safety

import "strings"

// protectedBrands maps the lowercased name or symbol of a well-known token to its
// canonical brand. A token presenting one of these identities is treated as an
// impersonation attempt. The list is not exhaustive; it blocks the obvious
// major-brand impersonations that a mint should never be allowed to create.
var protectedBrands = map[string]string{
	"usdc": "USDC", "usd coin": "USDC",
	"usdt": "USDT", "tether": "USDT",
	"sol": "SOL", "solana": "Solana", "wsol": "wSOL",
	"btc": "BTC", "bitcoin": "Bitcoin", "wbtc": "WBTC",
	"eth": "ETH", "ethereum": "Ethereum", "weth": "WETH",
	"bnb": "BNB", "dai": "DAI", "usds": "USDS",
	"bonk": "BONK", "jup": "JUP", "jupiter": "Jupiter",
	"pyth": "PYTH", "jito": "JITO", "jitosol": "JitoSOL",
}

// ImpersonationTarget returns the canonical brand a token's name or symbol
// impersonates, or "" if neither matches a protected brand. Matching is
// case-insensitive and ignores surrounding whitespace.
func ImpersonationTarget(name, symbol string) string {
	for _, s := range []string{name, symbol} {
		if brand, ok := protectedBrands[strings.ToLower(strings.TrimSpace(s))]; ok {
			return brand
		}
	}
	return ""
}
