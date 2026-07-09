package ids_test

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
)

// genMillis draws a millisecond timestamp anywhere in the 48-bit range the
// UUIDv7 layout can carry.
func genMillis(rt *rapid.T) int64 {
	return rapid.Int64Range(0, (1<<48)-1).Draw(rt, "ms")
}

// gen returns a Generator pinned to ms on the clock and to the drawn entropy
// stream, so every property runs over arbitrary times and arbitrary randomness.
func gen(rt *rapid.T, ms int64) *ids.Generator {
	entropy := rapid.SliceOfN(rapid.Byte(), 64, 64).Draw(rt, "entropy")
	return ids.NewGenerator(
		ids.WithClock(clock.NewManual(time.UnixMilli(ms))),
		ids.WithEntropy(bytes.NewReader(entropy)),
	)
}

// Property: every generated ID is canonical UUIDv7: 36 lowercase-hex characters
// in 8-4-4-4-12 form, version nibble 7, RFC 4122 variant, and the leading 48
// bits are exactly the clock's millisecond timestamp.
func TestProp_NewIsCanonicalUUIDv7(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ms := genMillis(rt)
		id := gen(rt, ms).New()

		if len(id) != 36 {
			rt.Fatalf("len(%q) = %d, want 36", id, len(id))
		}
		for _, i := range []int{8, 13, 18, 23} {
			if id[i] != '-' {
				rt.Fatalf("%q: byte %d = %q, want '-'", id, i, id[i])
			}
		}
		hexPart := strings.ReplaceAll(id, "-", "")
		if strings.ToLower(hexPart) != hexPart {
			rt.Fatalf("%q is not lowercase", id)
		}
		if id[14] != '7' {
			rt.Fatalf("%q: version nibble = %q, want '7'", id, id[14])
		}
		if !strings.ContainsRune("89ab", rune(id[19])) {
			rt.Fatalf("%q: variant nibble = %q, want one of 89ab", id, id[19])
		}
		gotMs, err := strconv.ParseInt(hexPart[:12], 16, 64)
		if err != nil {
			rt.Fatalf("parse timestamp of %q: %v", id, err)
		}
		if gotMs != ms {
			rt.Fatalf("%q carries ms %d, want %d", id, gotMs, ms)
		}
	})
}

// Property: IDs sort by creation time - for any two distinct millisecond
// timestamps, the earlier one's ID is lexicographically smaller, whatever the
// randomness. This is the index-locality guarantee the package exists for.
func TestProp_IDsSortByTime(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ms1 := genMillis(rt)
		ms2 := genMillis(rt)
		if ms1 == ms2 {
			return
		}
		if ms1 > ms2 {
			ms1, ms2 = ms2, ms1
		}
		id1 := gen(rt, ms1).New()
		id2 := gen(rt, ms2).New()
		if id1 >= id2 {
			rt.Fatalf("id at ms %d = %q not < id at ms %d = %q", ms1, id1, ms2, id2)
		}
	})
}

// Property: the same clock and the same entropy stream reproduce the same IDs
// in the same order - the determinism a replay depends on.
func TestProp_DeterministicUnderSameSeed(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ms := genMillis(rt)
		entropy := rapid.SliceOfN(rapid.Byte(), 40, 40).Draw(rt, "entropy")
		n := rapid.IntRange(1, 4).Draw(rt, "n")

		mk := func() []string {
			g := ids.NewGenerator(
				ids.WithClock(clock.NewManual(time.UnixMilli(ms))),
				ids.WithEntropy(bytes.NewReader(entropy)),
			)
			out := make([]string, n)
			for i := range out {
				out[i] = g.New()
			}
			return out
		}

		a, b := mk(), mk()
		for i := range a {
			if a[i] != b[i] {
				rt.Fatalf("run 1 id %d = %q, run 2 = %q; want identical", i, a[i], b[i])
			}
		}
	})
}

// Property: a Token is URL-safe unpadded base64 of exactly the requested number
// of entropy bytes, decoding back to the bytes the source produced; a
// non-positive size falls back to 256 bits.
func TestProp_TokenRoundTripsEntropy(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nBytes := rapid.IntRange(-2, 64).Draw(rt, "nBytes")
		want := nBytes
		if nBytes <= 0 {
			want = 32
		}
		entropy := rapid.SliceOfN(rapid.Byte(), want, want).Draw(rt, "entropy")

		g := ids.NewGenerator(ids.WithEntropy(bytes.NewReader(entropy)))
		tok, err := g.Token(nBytes)
		if err != nil {
			rt.Fatalf("Token(%d): %v", nBytes, err)
		}
		got, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			rt.Fatalf("token %q is not raw URL-safe base64: %v", tok, err)
		}
		if !bytes.Equal(got, entropy) {
			rt.Fatalf("token decodes to %x, want the source entropy %x", got, entropy)
		}
	})
}
