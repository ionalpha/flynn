package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/usage"
)

const testPrices = `{"prices":[{"provider":"acme","model":"m1","effective":"2026-01-01","currency":"USD","input":"3","output":"15","cacheRead":"0.3","cacheWrite":"3.75","source":"https://example.com/pricing read 2026-01-01"}]}`

func priceTable(t *testing.T) *usage.Table {
	t.Helper()
	tbl, err := usage.LoadTable(strings.NewReader(testPrices))
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

// A turn is priced from the tokens the provider reported and the table's row for that
// model; a model the table does not list is unpriced, never free.
func TestPricerPricesReportedTokensOrLeavesThemUnpriced(t *testing.T) {
	u := llm.Usage{InputTokens: 1_000_000, OutputTokens: 100_000}
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	got := newPricer(priceTable(t), "acme:m1").turn(u, at)
	if !got.Known() || got.Money.String() != "4.5" || got.Currency != "USD" {
		t.Errorf("priced turn = %+v, want 4.5 USD (3 for input, 1.5 for output)", got)
	}
	for name, p := range map[string]*pricer{
		"model not in the table": newPricer(priceTable(t), "acme:other"),
		"no table":               newPricer(nil, "acme:m1"),
		"no pricer":              nil,
	} {
		c := p.turn(u, at)
		if c.Known() || c.Unpriced != 1 || c.InputTokens != 1_000_000 || c.Money != 0 {
			t.Errorf("%s: %+v, want tokens kept and the turn unpriced", name, c)
		}
	}
	before := newPricer(priceTable(t), "acme:m1").turn(u, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if before.Known() {
		t.Error("a turn before the row's effective date was priced")
	}
}

func TestMeteredModelAccumulatesMoneyFromTheTable(t *testing.T) {
	m := newMeteredModel(&fakeModel{replies: []llm.Response{reply("a", 1_000_000, 0), reply("b", 1_000_000, 0)}}, newPricer(priceTable(t), "acme:m1"))
	for range 2 {
		if _, err := m.Generate(context.Background(), llm.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	cost, _, _ := m.snapshot()
	if !cost.Known() || cost.Money.String() != "6" {
		t.Errorf("cost = %+v, want 6 USD known", cost)
	}
}

func TestEndingOfTellsAStallAndABudgetStopFromAnError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want skillab.Ending
	}{
		{errors.New("goal stalled: no progress for 3 consecutive steps"), skillab.Stalled},
		{errors.New("budget_exceeded: step limit"), skillab.BudgetExhausted},
		{context.Canceled, skillab.Aborted},
		{errors.New("provider unavailable"), skillab.Aborted},
	} {
		if got := endingOf(tc.err); got != tc.want {
			t.Errorf("endingOf(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestSkillABSpendFlags(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(good, []byte(testPrices), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"prices":[{"provider":"x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	limit, table, err := abFlags{maxCost: "2", prices: good}.spend()
	if err != nil || limit.Money.String() != "2" || limit.Currency != "USD" || table == nil {
		t.Errorf("spend = %+v %v %v", limit, table, err)
	}
	if limit, table, err = (abFlags{}).spend(); err != nil || limit.Money != 0 || table != nil {
		t.Errorf("no flags = %+v %v %v, want no limit and no table", limit, table, err)
	}
	for name, f := range map[string]abFlags{
		"zero limit":        {maxCost: "0"},
		"negative":          {maxCost: "-1"},
		"not a number":      {maxCost: "two"},
		"unreadable prices": {prices: filepath.Join(dir, "absent.json")},
		"invalid prices":    {prices: bad},
	} {
		if _, _, err := f.spend(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestSkillABArgsReadSpendFlags(t *testing.T) {
	root := writeExerciseSet(t, "open-only", "do the thing | exit 0\n")
	var out bytes.Buffer
	_, _, flags, err := skillABArgs([]string{"open-only", "--exercises", root, "--max-cost", "0.50", "--prices", "p.json"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if flags.maxCost != "0.50" || flags.prices != "p.json" {
		t.Errorf("flags = %+v", flags)
	}
}

// The command stops starting attempts once the limit is spent and says so, with the
// pairs that ran still scored; a spend it cannot read stops it the same way.
func TestMeasureSkillReportsAStopHonestly(t *testing.T) {
	set := skillab.Set{Skill: "s"}
	for i := range 4 {
		set.Exercises = append(set.Exercises, skillab.Exercise{Objective: fmt.Sprintf("o%d", i), Verify: "exit 0"})
	}
	priced := func(context.Context, skillab.Exercise, int, bool) (skillab.Outcome, error) {
		return skillab.Outcome{Passed: true, Cost: skillab.Cost{Turns: 1, Money: 1_000_000_000, Currency: "USD"}}, nil
	}
	unpriced := func(context.Context, skillab.Exercise, int, bool) (skillab.Outcome, error) {
		return skillab.Outcome{Passed: true, Cost: skillab.Cost{Turns: 1, InputTokens: 7, Unpriced: 1}}, nil
	}
	limit := skillab.Limit{Money: 3_000_000_000, Currency: "USD"}

	var out bytes.Buffer
	if err := measureSkill(context.Background(), &out, set, 1, skillab.Arms{}, limit, priced); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "STOPPED EARLY after 2 of 4 pairs") {
		t.Errorf("a run cut short by its limit does not say so:\n%s", &out)
	}

	out.Reset()
	if err := measureSkill(context.Background(), &out, set, 1, skillab.Arms{}, limit, unpriced); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"STOPPED EARLY after 1 of 4 pairs", "cost unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("unpriced run does not say %q:\n%s", want, &out)
		}
	}
}
