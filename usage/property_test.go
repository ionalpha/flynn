package usage_test

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/usage"
)

// Property: an amount written by String reads back as the same amount, so a
// price table or a record survives being saved and loaded with no drift.
func TestProp_NanosRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := usage.Nanos(rapid.Int64Min(0).Draw(rt, "n"))
		got, err := usage.ParseNanos(n.String())
		if err != nil || got != n {
			rt.Fatalf("ParseNanos(%q) = %d, %v; want %d", n.String(), got, err, n)
		}
	})
}

// Property: a call is priced by the latest row effective on or before its UTC day,
// and by no row when it ran before the first one. The expectation is computed by
// scanning the rows, independently of the table's sorted search.
func TestProp_LookupPicksTheRowInForce(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rapid.Check(t, func(rt *rapid.T) {
		offsets := rapid.SliceOfNDistinct(rapid.IntRange(0, 365), 1, 8, rapid.ID[int]).Draw(rt, "days")
		var rows []usage.Price
		for i, d := range offsets {
			rows = append(rows, usage.Price{
				Provider: "acme", Model: "m", Effective: base.AddDate(0, 0, d), Currency: "USD",
				Input: usage.Nanos(i + 1), Output: 1, Source: "property test",
			})
		}
		tb, err := usage.NewTable(rows...)
		if err != nil {
			rt.Fatal(err)
		}
		at := base.Add(time.Duration(rapid.Int64Range(-48, 400*24).Draw(rt, "hours")) * time.Hour)

		var want *usage.Price
		for i := range rows {
			if !rows[i].Effective.After(at) && (want == nil || rows[i].Effective.After(want.Effective)) {
				want = &rows[i]
			}
		}
		got, ok := tb.Lookup("acme", "m", at)
		switch {
		case want == nil && ok:
			rt.Fatalf("at %s: got row %s; want none", at, got.ID())
		case want != nil && (!ok || got.Input != want.Input):
			rt.Fatalf("at %s: got %s (ok=%v); want %s", at, got.ID(), ok, want.ID())
		}
	})
}

// Property: pricing two calls separately and summing differs from pricing their
// combined tokens by at most one nano per charged figure, the rounding each
// separate charge can add. It bounds how far a per-call ledger can drift from a
// per-run bill.
func TestProp_CostIsAdditiveWithinRounding(t *testing.T) {
	cr, cw := usage.Nanos(300_000_000), usage.Nanos(3_750_000_000)
	tb, err := usage.NewTable(usage.Price{
		Provider: "acme", Model: "m", Effective: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Currency: "USD",
		Input: 3_000_000_000, Output: 15_000_000_000, CacheRead: &cr, CacheWrite: &cw, Source: "property test",
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tokens := func(rt *rapid.T, label string) usage.Tokens {
		in := rapid.IntRange(0, 2_000_000).Draw(rt, label+".in")
		return reported(in, rapid.IntRange(0, 200_000).Draw(rt, label+".out"),
			rapid.IntRange(0, in).Draw(rt, label+".cacheRead"), rapid.IntRange(0, 200_000).Draw(rt, label+".cacheWrite"))
	}
	rapid.Check(t, func(rt *rapid.T) {
		a, b := tokens(rt, "a"), tokens(rt, "b")
		var both usage.Tokens
		both.Input = usage.Count{N: a.Input.N + b.Input.N, Basis: usage.Reported}
		both.Output = usage.Count{N: a.Output.N + b.Output.N, Basis: usage.Reported}
		both.CacheRead = usage.Count{N: a.CacheRead.N + b.CacheRead.N, Basis: usage.Reported}
		both.CacheWrite = usage.Count{N: a.CacheWrite.N + b.CacheWrite.N, Basis: usage.Reported}

		ca, cb, cab := tb.Price(call("m", at, a)), tb.Price(call("m", at, b)), tb.Price(call("m", at, both))
		if !ca.Known || !cb.Known || !cab.Known {
			rt.Fatalf("unpriced: %+v %+v %+v", ca, cb, cab)
		}
		diff := ca.Amount + cb.Amount - cab.Amount
		if diff < -4 || diff > 4 {
			rt.Fatalf("separate %s + %s vs combined %s: drift %d nanos", ca.Amount, cb.Amount, cab.Amount, diff)
		}
	})
}
