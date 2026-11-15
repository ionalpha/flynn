// Package brakes halts a run from outside the model's reasoning. Its [Hook] sits on the
// dispatch waist, sees every model and tool call, and trips a breaker when a run calls
// tools too fast, repeats one action, fails repeatedly or spends past a ceiling.
//
// The brakes are wired by the host and are not a tool, so no action a run can take
// disables them, and an injected or misaligned run is braked like any other. The
// kill-switch and the breakers share one halt state per run, which refuses every later
// action until it is reset.
//
// The capability admitter decides what a run may do and the budget caps spend; the
// brakes catch patterns of activity over time that those checks cannot see.
package brakes

import (
	"context"
	"sync"
	"time"
)

// Limits configures the automatic breakers. A zero field disables its breaker, so the
// zero Limits leaves only the kill-switch and any anomaly detector.
type Limits struct {
	// MaxActions is the most actions a run may dispatch within the trailing Window.
	// Either one zero disables the rate breaker.
	MaxActions int
	Window     time.Duration

	// MaxRepeats is the most consecutive dispatches of one action name.
	MaxRepeats int

	// ErrorRate is the fraction of recent actions, in [0,1], that may fail.
	ErrorRate float64
	// ErrorRateMinSamples is both the outcomes needed before the error breaker may trip
	// and the window the rate is measured over. Zero means 5.
	ErrorRateMinSamples int

	// MaxTokens and MaxCost halt the whole run once cumulative metering reaches them,
	// where the budget refuses a single action.
	MaxTokens int64
	MaxCost   float64
}

// defaultErrorSamples keeps a couple of early failures from tripping the error breaker.
const defaultErrorSamples = 5

func (l Limits) active() bool {
	return (l.MaxActions > 0 && l.Window > 0) ||
		l.MaxRepeats > 0 ||
		l.ErrorRate > 0 ||
		l.MaxTokens > 0 ||
		l.MaxCost > 0
}

// Switch is the kill-switch: the per-run halt state an operator or a breaker engages.
// An implementation must be safe for concurrent use. A host whose halt must survive a
// restart or reach a fleet supplies a durable one in place of [MemSwitch].
type Switch interface {
	// Engage halts run. An already-halted run keeps its first reason, so the original
	// cause is not overwritten.
	Engage(run, reason string)
	Engaged(run string) (bool, string)
	// Reset is the operator's action to resume a halted run.
	Reset(run string)
}

// MemSwitch is the default Switch. A halt lasts for the life of the process.
type MemSwitch struct {
	mu      sync.Mutex
	reasons map[string]string
}

// NewMemSwitch returns a switch with no run halted.
func NewMemSwitch() *MemSwitch { return &MemSwitch{reasons: map[string]string{}} }

// Engage implements [Switch].
func (s *MemSwitch) Engage(run, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reasons[run]; ok {
		return
	}
	s.reasons[run] = reason
}

// Engaged implements [Switch].
func (s *MemSwitch) Engaged(run string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reasons[run]
	return ok, r
}

// Reset implements [Switch].
func (s *MemSwitch) Reset(run string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reasons, run)
}

var _ Switch = (*MemSwitch)(nil)

// AnomalyDetector lets an outside signal source, such as a syscall or audit feed that
// models a run's normal behaviour, halt the run without this package importing it.
type AnomalyDetector interface {
	// Observe is offered every completed action and returns true with a reason to halt
	// the run. It must be safe for concurrent use and must not block.
	Observe(ctx context.Context, action string, tokens int64, cost float64, failed bool) (halt bool, reason string)
}

type ctxKey struct{}

// Into binds the run id the brakes track. A fan-out sharing one id shares one set of
// brakes. An unbound context uses one shared bucket, so a missing id is still braked.
func Into(ctx context.Context, run string) context.Context {
	return context.WithValue(ctx, ctxKey{}, run)
}

// runFromContext returns the bound run id, or "" for the shared bucket.
func runFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKey{}).(string); ok {
		return id
	}
	return ""
}
