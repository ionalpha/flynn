package ids_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/ids"
)

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
