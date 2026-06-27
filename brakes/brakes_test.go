package brakes_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// counter is a work closure that records how many times it actually ran, so a test
// can prove a refused action never executed.
type counter struct{ ran int }

func (c *counter) work(m dispatch.Metering, err error) func(context.Context) (dispatch.Metering, error) {
	return func(context.Context) (dispatch.Metering, error) {
		c.ran++
		return m, err
	}
}

// govern dispatches one action named name through d, on a context scoped to run.
func govern(t *testing.T, d *dispatch.Dispatcher, run, name string, work func(context.Context) (dispatch.Metering, error)) error {
	t.Helper()
	ctx := brakes.Into(context.Background(), run)
	return d.Govern(ctx, dispatch.Action{Name: name}, work)
}

func assertHalted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the action to be refused, got nil")
	}
	if got := fault.Classify(err); got != fault.Forbidden {
		t.Fatalf("expected a Forbidden refusal, got %s: %v", got, err)
	}
}

func TestRateBreakerHaltsThroughTheWaist(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	h := brakes.NewHook(brakes.Limits{MaxActions: 3, Window: time.Minute}, nil, brakes.WithClock(clk))
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	for i := range 3 {
		if err := govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)); err != nil {
			t.Fatalf("action %d should be allowed: %v", i, err)
		}
	}
	// The fourth action within the window is one too many: it is refused and never runs.
	assertHalted(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	if c.ran != 3 {
		t.Fatalf("work ran %d times, want 3 (the over-limit action must not execute)", c.ran)
	}
	if halted, _ := h.Switch().Engaged("run"); !halted {
		t.Fatal("rate breaker should have engaged the kill-switch")
	}
}

func TestRateBreakerCountsOnlyTheWindow(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0))
	h := brakes.NewHook(brakes.Limits{MaxActions: 2, Window: time.Minute}, nil, brakes.WithClock(clk))
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	// Advance past the window so the earlier actions age out and the rate resets.
	clk.Advance(2 * time.Minute)
	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	if c.ran != 4 {
		t.Fatalf("work ran %d times, want 4 (aged-out actions should not count)", c.ran)
	}
}

func TestRepeatBreakerHalts(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{MaxRepeats: 3}, nil)
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	for range 3 {
		mustAllow(t, govern(t, d, "run", "same", c.work(dispatch.Metering{}, nil)))
	}
	assertHalted(t, govern(t, d, "run", "same", c.work(dispatch.Metering{}, nil)))
	if c.ran != 3 {
		t.Fatalf("work ran %d times, want 3", c.ran)
	}
}

func TestRepeatBreakerResetsOnDifferentAction(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{MaxRepeats: 2}, nil)
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	// Alternating names never build a streak past the limit.
	for i := range 10 {
		name := "a"
		if i%2 == 1 {
			name = "b"
		}
		mustAllow(t, govern(t, d, "run", name, c.work(dispatch.Metering{}, nil)))
	}
	if c.ran != 10 {
		t.Fatalf("work ran %d times, want 10 (alternating actions must not trip the repeat breaker)", c.ran)
	}
}

func TestTokenCeilingHalts(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{MaxTokens: 100}, nil)
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	// Two actions of 60 tokens cross the 100-token ceiling after the second; the
	// next action is then refused.
	mustAllow(t, govern(t, d, "run", "model", c.work(dispatch.Metering{Tokens: 60}, nil)))
	mustAllow(t, govern(t, d, "run", "model", c.work(dispatch.Metering{Tokens: 60}, nil)))
	assertHalted(t, govern(t, d, "run", "model", c.work(dispatch.Metering{Tokens: 60}, nil)))
	if c.ran != 2 {
		t.Fatalf("work ran %d times, want 2", c.ran)
	}
}

func TestErrorRateBreakerHalts(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{ErrorRate: 0.5, ErrorRateMinSamples: 4}, nil)
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}
	boom := errors.New("boom")

	// Four actions, half of them failing, reaches the 50% error rate at the sample
	// floor; the run is then halted.
	_ = govern(t, d, "run", "tool", c.work(dispatch.Metering{}, boom))
	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	_ = govern(t, d, "run", "tool", c.work(dispatch.Metering{}, boom))
	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	assertHalted(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
}

func TestOperatorKillSwitchBlocksCleanlyAndResets(t *testing.T) {
	sw := brakes.NewMemSwitch()
	h := brakes.NewHook(brakes.Limits{}, sw) // no breakers; only the operator switch
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))

	// An operator (or the control plane) engages the kill-switch out of band.
	sw.Engage("run", "operator stop")
	assertHalted(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	if c.ran != 1 {
		t.Fatalf("work ran %d times, want 1 (no work after the kill-switch)", c.ran)
	}

	// Resetting the switch is the deliberate resume.
	sw.Reset("run")
	mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil)))
	if c.ran != 2 {
		t.Fatalf("work ran %d times, want 2 after reset", c.ran)
	}
}

func TestKillSwitchKeepsFirstReason(t *testing.T) {
	sw := brakes.NewMemSwitch()
	sw.Engage("run", "first")
	sw.Engage("run", "second")
	halted, reason := sw.Engaged("run")
	if !halted || reason != "first" {
		t.Fatalf("expected the first reason to stick, got halted=%v reason=%q", halted, reason)
	}
}

// stubDetector halts a run the moment it sees a named action, standing in for an
// out-of-band signal source.
type stubDetector struct{ trigger string }

func (s stubDetector) Observe(_ context.Context, action string, _ int64, _ float64, _ bool) (bool, string) {
	if action == s.trigger {
		return true, "out-of-pattern access"
	}
	return false, ""
}

func TestAnomalyDetectorHalts(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{}, nil, brakes.WithAnomalyDetector(stubDetector{trigger: "drop_table"}))
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	mustAllow(t, govern(t, d, "run", "select", c.work(dispatch.Metering{}, nil)))
	// The anomalous action itself runs (the detector observes it after the fact),
	// but it engages the halt so the next action is refused.
	mustAllow(t, govern(t, d, "run", "drop_table", c.work(dispatch.Metering{}, nil)))
	assertHalted(t, govern(t, d, "run", "anything", c.work(dispatch.Metering{}, nil)))
}

func TestUnbrakedRunIsUnconstrained(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{}, nil) // nothing configured, no switch engaged
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	for range 50 {
		mustAllow(t, govern(t, d, "run", "tool", c.work(dispatch.Metering{Tokens: 1000}, nil)))
	}
	if c.ran != 50 {
		t.Fatalf("work ran %d times, want 50 (an unbraked run is unconstrained)", c.ran)
	}
}

func TestRunsAreBrakedIndependently(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{MaxRepeats: 2}, nil)
	d := dispatch.New(dispatch.WithHook(h))
	a, b := &counter{}, &counter{}

	// Halt run A by repeating its action past the limit.
	mustAllow(t, govern(t, d, "A", "x", a.work(dispatch.Metering{}, nil)))
	mustAllow(t, govern(t, d, "A", "x", a.work(dispatch.Metering{}, nil)))
	assertHalted(t, govern(t, d, "A", "x", a.work(dispatch.Metering{}, nil)))

	// Run B is unaffected by A's halt.
	mustAllow(t, govern(t, d, "B", "x", b.work(dispatch.Metering{}, nil)))
	if b.ran != 1 {
		t.Fatalf("run B work ran %d times, want 1 (one run's halt must not brake another)", b.ran)
	}
}

// TestRedTeamJailbrokenRunIsStoppedAndCannotBypass models a run that ignores the
// refusal and keeps dispatching, including under action names that read like an
// attempt to disable the brake. The waist refuses every one, the work stops, and
// no name slips through: the run cannot reason or rename its way past the brake
// because the brake is not on the tool surface it can reach.
func TestRedTeamJailbrokenRunIsStoppedAndCannotBypass(t *testing.T) {
	h := brakes.NewHook(brakes.Limits{MaxActions: 5, Window: time.Hour}, nil)
	d := dispatch.New(dispatch.WithHook(h))
	c := &counter{}

	allowed := 0
	bypassNames := []string{"tool", "reset_brakes", "disable_safety", "kill_switch_off", "admin_override"}
	for i := range 100 {
		name := bypassNames[i%len(bypassNames)]
		if err := govern(t, d, "run", name, c.work(dispatch.Metering{}, nil)); err == nil {
			allowed++
		}
	}
	if allowed > 5 {
		t.Fatalf("%d actions ran past the rate breaker; a jailbroken run must be capped at the limit", allowed)
	}
	if c.ran != allowed {
		t.Fatalf("work ran %d times but %d were allowed; a refused action must not execute", c.ran, allowed)
	}
	if halted, _ := h.Switch().Engaged("run"); !halted {
		t.Fatal("the run should remain halted; no action name bypassed the brake")
	}
}

// TestRateBreakerInvariantProperty is the rigor property: over a window long
// enough that no action ages out, the rate breaker admits exactly the first
// MaxActions of any sequence and refuses every action after, so the count a run
// can ever take is capped at the limit and the halt is sticky (no action runs
// after a refusal). This holds for any limit and any sequence length.
func TestRateBreakerInvariantProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		maxActions := rapid.IntRange(1, 20).Draw(rt, "maxActions")
		n := rapid.IntRange(0, 60).Draw(rt, "actions")

		// A window far larger than the test means nothing ages out, so the breaker
		// sees the whole sequence as one burst.
		h := brakes.NewHook(brakes.Limits{MaxActions: maxActions, Window: time.Hour}, nil)
		d := dispatch.New(dispatch.WithHook(h))
		c := &counter{}

		allowed, refusedYet := 0, false
		for range n {
			err := govern(t, d, "run", "tool", c.work(dispatch.Metering{}, nil))
			if err == nil {
				if refusedYet {
					rt.Fatal("an action was allowed after a refusal; the halt must be sticky")
				}
				allowed++
				continue
			}
			refusedYet = true
			if got := fault.Classify(err); got != fault.Forbidden {
				rt.Fatalf("refusal class = %s, want forbidden", got)
			}
		}

		want := n
		if want > maxActions {
			want = maxActions
		}
		if allowed != want {
			rt.Fatalf("allowed = %d, want %d (max=%d, n=%d)", allowed, want, maxActions, n)
		}
		if c.ran != allowed {
			rt.Fatalf("work ran %d times but %d were allowed; a refused action must not execute", c.ran, allowed)
		}
	})
}

func mustAllow(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("action should have been allowed: %v", err)
	}
}
