package ids_test

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
)

// TestEntropyHandsBackTheGeneratorsSource: a caller that needs raw bytes rather than an
// identifier takes them from the same source every identifier comes from. A deterministic
// generator hands back its deterministic reader, which is what keeps a replay a replay all
// the way down to a minted key; a nil generator falls back to the package default rather
// than to a second, un-injectable source of randomness.
func TestEntropyHandsBackTheGeneratorsSource(t *testing.T) {
	fixed := bytes.NewReader(bytes.Repeat([]byte{0xab}, 64))
	g := ids.NewGenerator(ids.WithEntropy(fixed))
	if ids.Entropy(g) != io.Reader(fixed) {
		t.Fatal("Entropy did not hand back the generator's own source")
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(ids.Entropy(g), got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, []byte{0xab, 0xab, 0xab, 0xab}) {
		t.Fatalf("read %x from the injected source, want the bytes it holds", got)
	}
	if ids.Entropy(nil) == nil {
		t.Fatal("the package default has no entropy source")
	}
}

func TestUUIDv7Format(t *testing.T) {
	id := ids.New()
	if len(id) != 36 {
		t.Fatalf("len = %d, want 36 (%q)", len(id), id)
	}
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			t.Fatalf("expected '-' at %d, got %q in %q", pos, id[pos], id)
		}
	}
	if id[14] != '7' {
		t.Fatalf("version nibble = %q, want '7' (%q)", id[14], id)
	}
	if !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("variant nibble = %q, want one of 8/9/a/b (%q)", id[19], id)
	}
}

func TestSortableByTime(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1_000_000))
	g := ids.NewGenerator(ids.WithClock(clk))

	earlier := g.New()
	clk.Advance(time.Second)
	later := g.New()

	if earlier >= later {
		t.Fatalf("a later-timestamped id must sort after an earlier one: %q vs %q", earlier, later)
	}
}

func TestDeterministicUnderInjectedSources(t *testing.T) {
	clk := clock.NewManual(time.UnixMilli(1_700_000_000_000))
	seed := []byte("deterministic-entropy-bytes!!")

	a := ids.NewGenerator(ids.WithClock(clk), ids.WithEntropy(bytes.NewReader(seed))).New()
	b := ids.NewGenerator(ids.WithClock(clk), ids.WithEntropy(bytes.NewReader(seed))).New()

	if a != b {
		t.Fatalf("same clock + entropy must reproduce the same id: %q vs %q", a, b)
	}
}

func TestUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id := ids.New()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTokenEntropyAndEncoding(t *testing.T) {
	tok, err := ids.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	// 256 bits -> 32 bytes -> 43 chars of raw (unpadded) URL-safe base64.
	if len(tok) != 43 {
		t.Errorf("default token len = %d, want 43", len(tok))
	}
	// URL-safe, no padding: only [A-Za-z0-9_-], never '+', '/', or '='.
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token %q is not URL-safe/unpadded", tok)
	}
}

func TestTokenUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		tok, err := ids.Token()
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token: %q", tok)
		}
		seen[tok] = struct{}{}
	}
}

func TestTokenDeterministicUnderInjectedEntropy(t *testing.T) {
	seed := bytes.Repeat([]byte("entropy!"), 16)
	a, err := ids.NewGenerator(ids.WithEntropy(bytes.NewReader(seed))).Token(32)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ids.NewGenerator(ids.WithEntropy(bytes.NewReader(seed))).Token(32)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("same entropy must reproduce the same token: %q vs %q", a, b)
	}
}

func TestTokenNonPositiveSizeUsesDefault(t *testing.T) {
	tok, err := ids.NewGenerator(ids.WithEntropy(bytes.NewReader(bytes.Repeat([]byte("x"), 64)))).Token(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 {
		t.Errorf("non-positive size should fall back to 256-bit token, got len %d", len(tok))
	}
}
