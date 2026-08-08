package hybrid

import "testing"

func TestCosineEdges(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"scaled", []float32{1, 2, 3}, []float32{2, 4, 6}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposed", []float32{1, 0}, []float32{-1, 0}, -1},
		// A corpus half re-embedded under a model of a different width degrades to
		// no opinion, rather than failing every recall while the backfill runs.
		{"different widths", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"empty", nil, nil, 0},
		{"zero vector", []float32{0, 0}, []float32{1, 1}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cosine(tc.a, tc.b); got != tc.want {
				t.Fatalf("cosine = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVecCacheEvictsOldestFirst(t *testing.T) {
	c := newVecCache(2)
	c.put("a", []float32{1})
	c.put("b", []float32{2})
	c.put("a", []float32{9}) // already held: the first vector stands and does not re-queue
	c.put("c", []float32{3})

	if _, ok := c.get("a"); ok {
		t.Fatal("cache kept the oldest entry over the cap")
	}
	for _, k := range []string{"b", "c"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("cache dropped %q, want the two newest kept", k)
		}
	}

	// Shrinking the cap evicts down to it immediately, rather than waiting for the
	// next write to notice.
	c.resize(1)
	if _, ok := c.get("b"); ok {
		t.Fatal("resize left the cache over its new cap")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("resize evicted the newest entry")
	}
}
