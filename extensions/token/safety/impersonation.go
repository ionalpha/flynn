package safety

import (
	"strings"
	"unicode"
)

// protectedBrands maps the normalized name or symbol of a well-known token to its
// canonical brand. A token presenting one of these identities is treated as an
// impersonation attempt. The keys are written in readable form; they are compared after
// the same normalization applied to the input (see normalize), so "usd coin" here matches
// "USD Coin", "USD-Coin", and "usdcoin". The list is not exhaustive; it blocks the obvious
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

// normalizedBrands is protectedBrands keyed by the normalized form, built once so a lookup
// is a single map hit against the same canonicalization the input goes through.
var normalizedBrands = func() map[string]string {
	m := make(map[string]string, len(protectedBrands))
	for k, v := range protectedBrands {
		if n := normalize(k); n != "" {
			m[n] = v
		}
	}
	return m
}()

// confusables maps common Cyrillic and Greek homoglyphs to the ASCII letter they visually
// imitate, so "Tethеr" (Cyrillic е) and "USDТ" (Cyrillic Т) fold to their Latin form and
// cannot slip past an ASCII blocklist. Only cross-script look-alikes are mapped, never
// digits: a digit has legitimate meaning in a token name (for example "ETH2"), so folding
// digits to letters would wrongly block distinct names.
var confusables = map[rune]rune{
	// Cyrillic lower/upper look-alikes.
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y',
	'х': 'x', 'к': 'k', 'н': 'h', 'в': 'b', 'т': 't', 'м': 'm',
	'А': 'a', 'Е': 'e', 'О': 'o', 'Р': 'p', 'С': 'c', 'У': 'y',
	'Х': 'x', 'К': 'k', 'Н': 'h', 'В': 'b', 'Т': 't', 'М': 'm',
	// Greek look-alikes.
	'ο': 'o', 'Ο': 'o', 'α': 'a', 'ρ': 'p', 'τ': 't', 'υ': 'y',
	'ι': 'i', 'κ': 'k', 'ν': 'v',
}

// normalize folds a name or symbol to a comparable canonical form: it drops zero-width and
// other format characters, maps cross-script homoglyphs to their ASCII look-alike,
// lowercases, and keeps only the resulting ASCII letters and digits (dropping spaces,
// hyphens, dots, and punctuation). So "U​S D-Coin" (with a zero-width space), "USD Coin",
// and "usdcoin" all fold to "usdcoin", closing the homoglyph, zero-width, and separator
// bypasses of an exact-string blocklist without folding away meaningful digits.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			continue // zero-width joiners/spaces and other format characters
		}
		if m, ok := confusables[r]; ok {
			r = m
		}
		r = unicode.ToLower(r)
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ImpersonationTarget returns the canonical brand a token's name or symbol impersonates, or
// "" if neither matches a protected brand. Matching is homoglyph-, zero-width-, case-, and
// separator-insensitive (see normalize), so look-alike spoofs of a protected brand are
// caught, not just an exact string.
func ImpersonationTarget(name, symbol string) string {
	for _, s := range []string{name, symbol} {
		if n := normalize(s); n != "" {
			if brand, ok := normalizedBrands[n]; ok {
				return brand
			}
		}
	}
	return ""
}
