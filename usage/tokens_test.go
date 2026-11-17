package usage_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/usage"
)

func TestTokensAddSumsEachFieldAndWeakensBasis(t *testing.T) {
	a := reported(1000, 50, 400, 20)
	b := reported(500, 25, 100, 0)
	got := a.Add(b)
	if got.Input.N != 1500 || got.Output.N != 75 || got.CacheRead.N != 500 || got.CacheWrite.N != 20 {
		t.Fatalf("Add = %+v, want input 1500 output 75 cacheRead 500 cacheWrite 20", got)
	}
	if got.Total() != a.Total()+b.Total() {
		t.Errorf("Total of the sum = %d, want the sum of the totals %d", got.Total(), a.Total()+b.Total())
	}
	if got.Input.Basis != usage.Derived {
		t.Errorf("a sum of reported figures has basis %q, want derived", got.Input.Basis)
	}
	if a.Input.N != 1000 {
		t.Errorf("Add changed its receiver: input %d", a.Input.N)
	}

	est := a
	est.Output = usage.Count{N: 5, Basis: usage.Estimated, Method: "recounted"}
	if w := a.Add(est).Output; w.Basis != usage.Estimated || w.Method == "" {
		t.Errorf("a sum including an estimate = %+v, want estimated with a method", w)
	}
}

func TestCacheHitPercent(t *testing.T) {
	for name, tc := range map[string]struct {
		tk   usage.Tokens
		want int
	}{
		"no input":             {reported(0, 10, 0, 0), 0},
		"no cache read":        {reported(1000, 10, 0, 50), 0},
		"all cached":           {reported(1000, 10, 1000, 0), 100},
		"rounds down":          {reported(3, 1, 2, 0), 66},
		"writes are not input": {reported(1000, 10, 900, 400), 90},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.tk.CacheHitPercent(); got != tc.want {
				t.Errorf("CacheHitPercent = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTokensAddTotalIsAdditiveProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		gen := func(label string) usage.Tokens {
			in := rapid.IntRange(0, 5_000_000).Draw(rt, label+".in")
			return reported(in, rapid.IntRange(0, 100_000).Draw(rt, label+".out"),
				rapid.IntRange(0, in).Draw(rt, label+".cr"), rapid.IntRange(0, 100_000).Draw(rt, label+".cw"))
		}
		a, b := gen("a"), gen("b")
		sum := a.Add(b)
		if sum.Total() != a.Total()+b.Total() {
			rt.Fatalf("Total(a+b) = %d, want %d", sum.Total(), a.Total()+b.Total())
		}
		if err := sum.Validate(); err != nil {
			rt.Fatalf("a sum of valid tokens is invalid: %v", err)
		}
	})
}
