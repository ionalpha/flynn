package brakes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// Hook is the brakes' [dispatch.Hook]. Before refuses an action on a halted run and
// counts the attempt for the rate and repeat breakers. After counts outcome and cost for
// the error and spend breakers and the anomaly detector, which halt the next action. A
// tripped breaker engages the same kill-switch an operator does.
type Hook struct {
	limits   Limits
	sw       Switch
	detector AnomalyDetector
	clk      clock.Clock
	// refuse overrides the refusal so a test can show ProveHalts catches a wrong class.
	// Nil is the shipped refusal.
	refuse func(reason string) error

	mu    sync.Mutex
	state map[string]*runState
}

// Option configures a Hook.
type Option func(*Hook)

// WithClock sets the rate breaker's time source (default: clock.System).
func WithClock(c clock.Clock) Option { return func(h *Hook) { h.clk = c } }

// WithAnomalyDetector wires a detector consulted after each action (default: none).
func WithAnomalyDetector(d AnomalyDetector) Option { return func(h *Hook) { h.detector = d } }

// NewHook returns a Hook enforcing limits on sw, for [dispatch.WithHook]. A nil sw is
// replaced with a [MemSwitch], so the Hook always has a halt state.
func NewHook(limits Limits, sw Switch, opts ...Option) *Hook {
	if sw == nil {
		sw = NewMemSwitch()
	}
	h := &Hook{
		limits: limits,
		sw:     sw,
		clk:    clock.System{},
		state:  map[string]*runState{},
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Switch returns the kill-switch the breakers trip, for an operator to read or reset.
func (h *Hook) Switch() Switch { return h.sw }

// Before refuses an action on a halted run. The action that crosses a rate or repeat
// threshold is itself refused, so the run stops at the limit.
func (h *Hook) Before(ctx context.Context, a dispatch.Action) error {
	run := runFromContext(ctx)
	if halted, reason := h.sw.Engaged(run); halted {
		return h.halt(reason)
	}
	if !h.limits.active() {
		return nil
	}

	now := h.clk.Now()
	st := h.runState(run)

	st.mu.Lock()
	defer st.mu.Unlock()

	if h.limits.MaxActions > 0 && h.limits.Window > 0 {
		st.pruneWindow(now.Add(-h.limits.Window))
		st.window = append(st.window, now)
		if len(st.window) > h.limits.MaxActions {
			return h.trip(run, fmt.Sprintf("action rate exceeded: more than %d actions in %s",
				h.limits.MaxActions, h.limits.Window))
		}
	}

	if h.limits.MaxRepeats > 0 {
		if a.Name == st.lastAction {
			st.repeatStreak++
		} else {
			st.lastAction = a.Name
			st.repeatStreak = 1
		}
		if st.repeatStreak > h.limits.MaxRepeats {
			return h.trip(run, fmt.Sprintf("action %q repeated more than %d times in a row",
				a.Name, h.limits.MaxRepeats))
		}
	}

	return nil
}

// After accounts cost and outcome. The action has already run, so a trip here stops the
// next one.
func (h *Hook) After(ctx context.Context, a dispatch.Action, m dispatch.Metering, err error) {
	run := runFromContext(ctx)
	if h.limits.active() {
		h.account(run, m, err != nil)
	}
	if h.detector != nil {
		if halt, reason := h.detector.Observe(ctx, a.Name, int64(m.Tokens), m.Cost, err != nil); halt {
			h.sw.Engage(run, "anomaly: "+reason)
		}
	}
}

func (h *Hook) account(run string, m dispatch.Metering, failed bool) {
	st := h.runState(run)

	st.mu.Lock()
	defer st.mu.Unlock()

	st.tokens += int64(m.Tokens)
	st.cost += m.Cost
	if h.limits.MaxTokens > 0 && st.tokens >= h.limits.MaxTokens {
		h.sw.Engage(run, fmt.Sprintf("token ceiling reached: %d", st.tokens))
		return
	}
	if h.limits.MaxCost > 0 && st.cost >= h.limits.MaxCost {
		h.sw.Engage(run, fmt.Sprintf("cost ceiling reached: %.4f", st.cost))
		return
	}

	if h.limits.ErrorRate > 0 {
		samples := h.limits.ErrorRateMinSamples
		if samples <= 0 {
			samples = defaultErrorSamples
		}
		st.recordOutcome(failed, samples)
		if len(st.outcomes) >= samples && st.errorRate() >= h.limits.ErrorRate {
			h.sw.Engage(run, fmt.Sprintf("error rate %.0f%% over last %d actions",
				st.errorRate()*100, len(st.outcomes)))
		}
	}
}

// trip halts run and refuses the action that crossed the breaker.
func (h *Hook) trip(run, reason string) error {
	h.sw.Engage(run, reason)
	return h.halt(reason)
}

// halt refuses with Forbidden, because the fix is a reset and a retry cannot succeed.
func (h *Hook) halt(reason string) error {
	if h.refuse != nil {
		return h.refuse(reason)
	}
	return fault.New(fault.Forbidden, "run_halted", "run halted by safety brake: "+reason)
}

func (h *Hook) runState(run string) *runState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[run]
	if !ok {
		st = &runState{}
		h.state[run] = st
	}
	return st
}

// runState is one run's breaker bookkeeping, with its own mutex so concurrent actions
// in a fan-out account safely.
type runState struct {
	mu sync.Mutex

	window []time.Time // action times within the rate window

	lastAction   string
	repeatStreak int

	tokens int64
	cost   float64

	outcomes []bool // recent failure flags, newest last
}

// pruneWindow drops action times at or before cutoff.
func (s *runState) pruneWindow(cutoff time.Time) {
	keep := 0
	for _, t := range s.window {
		if t.After(cutoff) {
			s.window[keep] = t
			keep++
		}
	}
	s.window = s.window[:keep]
}

// recordOutcome keeps the last window flags, so the rate covers recent actions only.
func (s *runState) recordOutcome(failed bool, window int) {
	s.outcomes = append(s.outcomes, failed)
	if len(s.outcomes) > window {
		s.outcomes = s.outcomes[len(s.outcomes)-window:]
	}
}

func (s *runState) errorRate() float64 {
	if len(s.outcomes) == 0 {
		return 0
	}
	failed := 0
	for _, f := range s.outcomes {
		if f {
			failed++
		}
	}
	return float64(failed) / float64(len(s.outcomes))
}

var _ dispatch.Hook = (*Hook)(nil)
