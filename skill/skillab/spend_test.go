package skillab_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/usage"
)

const usd = "USD"

// tokens is a reported input and output count, as an adapter would give it.
func tokens(in, out int) usage.Tokens {
	return usage.FromLLM(llm.Usage{InputTokens: in, OutputTokens: out})
}

// priced is one turn costing amount, so a pair of two arms costs twice that.
func priced(amount usage.Nanos) skillab.Cost {
	return skillab.Cost{Turns: 1, Tokens: tokens(100, 10), Money: amount, Currency: usd}
}

func cheap(amount usage.Nanos) skillab.Trial {
	return func(context.Context, skillab.Exercise, int, bool) (skillab.Outcome, error) {
		return skillab.Outcome{Passed: true, Cost: priced(amount)}, nil
	}
}

// The limit is read between pairs: a pair that would pass it still runs whole, and the
// pair after it does not start. The report says it stopped, and how many were left.
func TestSpendLimitStopsBeforeTheNextPair(t *testing.T) {
	// Each pair costs 2 (one per arm); the limit is 3, so pair 2 starts (spent 2) and
	// pair 3 does not (spent 4).
	limit := skillab.Limit{Money: 3 * usage.Nanos(1_000_000_000), Currency: usd}
	rep, err := skillab.CompareWithin(context.Background(), set(5), 1, skillab.Arms{Candidate: "a", Baseline: "b"}, cheap(1_000_000_000), limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 2 {
		t.Fatalf("ran %d pairs, want 2", len(rep.Pairs))
	}
	if rep.Stopped == "" || rep.Planned != 5 {
		t.Fatalf("stopped %q planned %d", rep.Stopped, rep.Planned)
	}
	out := rep.String()
	for _, want := range []string{"STOPPED EARLY after 2 of 5 pairs", "spend limit 3 USD reached", "only the pairs that ran"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
}

func TestNoLimitRunsEveryPairAndSaysNothingOfStopping(t *testing.T) {
	rep, err := skillab.CompareWithin(context.Background(), set(3), 1, skillab.Arms{Candidate: "a", Baseline: "b"}, cheap(1_000_000_000), skillab.Limit{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 3 || rep.Stopped != "" {
		t.Fatalf("pairs %d stopped %q", len(rep.Pairs), rep.Stopped)
	}
	if strings.Contains(rep.String(), "STOPPED") {
		t.Errorf("a complete run claims to have stopped:\n%s", rep)
	}
}

// A limit that cannot be read is not assumed to hold: turns with no price stop the run
// after the first pair, and the report says why instead of showing a cost of zero.
func TestSpendLimitStopsWhenCostIsUnknown(t *testing.T) {
	trial := func(context.Context, skillab.Exercise, int, bool) (skillab.Outcome, error) {
		return skillab.Outcome{Passed: true, Cost: skillab.Cost{Turns: 1, Tokens: tokens(50, 0), Unpriced: 1}}, nil
	}
	limit := skillab.Limit{Money: 1_000_000_000, Currency: usd}
	rep, err := skillab.CompareWithin(context.Background(), set(4), 1, skillab.Arms{Candidate: "a", Baseline: "b"}, trial, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 1 {
		t.Fatalf("ran %d pairs, want 1", len(rep.Pairs))
	}
	out := rep.String()
	for _, want := range []string{"could not be checked", "cost unknown (1 of 1 turns had no price)", "50 tokens"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
	if rep.CandidateCost.Known() {
		t.Error("a cost with unpriced turns reads as known")
	}
}

func TestMixedCurrenciesAreNotSummed(t *testing.T) {
	sum := priced(5).Add(skillab.Cost{Turns: 2, Money: 7, Currency: "EUR"})
	if sum.Known() || sum.Unpriced != 2 {
		t.Errorf("USD plus EUR is known=%v unpriced=%d, want unknown with 2 unpriced", sum.Known(), sum.Unpriced)
	}
}

func TestReportStatesCostPerArmAndTheDifference(t *testing.T) {
	trial := func(_ context.Context, _ skillab.Exercise, _ int, candidate bool) (skillab.Outcome, error) {
		amount := usage.Nanos(1_000_000_000)
		if candidate {
			amount = 3_000_000_000
		}
		return skillab.Outcome{Passed: true, Cost: priced(amount)}, nil
	}
	rep, err := skillab.Compare(context.Background(), set(2), 1, skillab.Arms{Candidate: "v2", Baseline: "v1"}, trial)
	if err != nil {
		t.Fatal(err)
	}
	out := rep.String()
	for _, want := range []string{"3 USD", "1 USD", "cost difference: +4 USD for the candidate"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
}

// A stalled or budget-exhausted run is kept apart from a plain failure in the report.
func TestEndingsAreCountedPerArm(t *testing.T) {
	trial := func(_ context.Context, _ skillab.Exercise, repeat int, candidate bool) (skillab.Outcome, error) {
		switch {
		case candidate && repeat == 1:
			return skillab.Outcome{Ended: skillab.Stalled}, nil
		case !candidate && repeat == 2:
			return skillab.Outcome{Ended: skillab.BudgetExhausted}, nil
		case !candidate && repeat == 3:
			return skillab.Outcome{Ended: skillab.BudgetExhausted}, nil
		}
		return skillab.Outcome{Passed: true}, nil
	}
	rep, err := skillab.Compare(context.Background(), set(1), 3, skillab.Arms{Candidate: "v2", Baseline: "v1"}, trial)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CandidateEnds[skillab.Stalled] != 1 || rep.BaselineEnds[skillab.BudgetExhausted] != 2 {
		t.Fatalf("ends %v / %v", rep.CandidateEnds, rep.BaselineEnds)
	}
	out := rep.String()
	for _, want := range []string{"runs that did not finish, v2: 1 stalled", "runs that did not finish, v1: 2 budget exhausted"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
}
