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

// Hook is the brakes' integration with the dispatch waist. Before refuses an
// action when the run is halted, and also accounts the attempt so a rate or
// repeat breaker can halt the run before the action runs. After accounts the
// outcome and cost so the error and spend breakers, and any anomaly detector, can
// halt the run before its next action. Because the waist governs every action, one
// Hook brakes the whole run with no per-call wiring.
//
// A breaker that trips engages the run's kill-switch, so the automatic halt and an
// operator-invoked halt are the same state: once engaged, every later action that
// run dispatches is refused until the switch is reset.
type Hook struct {
	limits   Limits
	sw       Switch
	detector AnomalyDetector
	clk      clock.Clock

	mu    sync.Mutex
	state map[string]*runState
}

// Option configures a Hook.
type Option func(*Hook)

// WithClock sets the time source the rate breaker measures its window against
// (default: clock.System).
func WithClock(c clock.Clock) Option { return func(h *Hook) { h.clk = c } }

// WithAnomalyDetector wires a behavioural-anomaly detector the Hook consults after
// each action (default: none).
func WithAnomalyDetector(d AnomalyDetector) Option { return func(h *Hook) { h.detector = d } }

// NewHook returns a brakes Hook that enforces limits and trips sw. Add it to a
// dispatcher with dispatch.WithHook to brake every action that dispatcher governs.
// A nil sw is replaced with a fresh in-memory switch, so the Hook is always backed
// by a real halt state.
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

// Switch returns the kill-switch the Hook enforces, so an operator (or the
// control plane) can Engage, Engaged, or Reset a run's halt through the same state
// the breakers trip.
func (h *Hook) Switch() Switch { return h.sw }

// Before refuses an action when the run is halted, and otherwise records the
// attempt and trips the rate or repeat breaker if this action crosses it. The
// action that crosses a threshold is the one refused, so the breaker stops the run
// at the limit rather than one past it.
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

	// Rate: count this attempt within the trailing window and halt if it is one
	// too many.
	if h.limits.MaxActions > 0 && h.limits.Window > 0 {
		st.pruneWindow(now.Add(-h.limits.Window))
		st.window = append(st.window, now)
		if len(st.window) > h.limits.MaxActions {
			return h.trip(run, fmt.Sprintf("action rate exceeded: more than %d actions in %s",
				h.limits.MaxActions, h.limits.Window))
		}
	}

	// Repeat: count consecutive dispatches of the same action name and halt when
	// the run hammers one action.
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

// After accounts the action's cost and outcome, then halts the run when a spend or
// error breaker, or the anomaly detector, fires. The action has already run, so a
// trip here stops the run's next action rather than this one.
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

// account folds one completed action into the run's spend and error windows and
// trips the spend or error breaker when it crosses a ceiling.
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

// trip engages the kill-switch for run and returns the refusal for the action that
// crossed the breaker, so the same call both halts the run and is itself stopped.
func (h *Hook) trip(run, reason string) error {
	h.sw.Engage(run, reason)
	return h.halt(reason)
}

// halt is the refusal returned for an action on a halted run. It is Forbidden, a
// policy denial that must not be retried: the fix is to reset the kill-switch, not
// to try again.
func (h *Hook) halt(reason string) error {
	return fault.New(fault.Forbidden, "run_halted", "run halted by safety brake: "+reason)
}

// runState returns the per-run breaker state, creating it on first use.
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

// runState is one run's breaker bookkeeping: the recent action times for the rate
// breaker, the consecutive-repeat streak, the spend totals, and the recent
// outcomes for the error breaker. Its own mutex guards it so concurrent actions in
// a fan-out account safely.
type runState struct {
	mu sync.Mutex

	window []time.Time // action times within the rate breaker's trailing window

	lastAction   string // the most recent action name, for the repeat breaker
	repeatStreak int    // consecutive dispatches of lastAction

	tokens int64   // cumulative metered tokens
	cost   float64 // cumulative metered cost

	outcomes []bool // recent failure flags, newest last, capped to the error window
}

// pruneWindow drops action times at or before cutoff, so the rate breaker counts
// only the trailing window.
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

// recordOutcome appends a failure flag, keeping only the last window entries so
// the error rate is measured over a trailing window rather than the whole run.
func (s *runState) recordOutcome(failed bool, window int) {
	s.outcomes = append(s.outcomes, failed)
	if len(s.outcomes) > window {
		s.outcomes = s.outcomes[len(s.outcomes)-window:]
	}
}

// errorRate is the fraction of recorded outcomes that failed.
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
