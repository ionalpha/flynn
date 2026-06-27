// Package brakes is the run's safety governor: a control loop that halts a run
// from outside the model's reasoning. It hangs off the dispatch waist as a Hook,
// so it observes every model and tool call a run makes and can stop the run on
// what it observes, regardless of what the model decided to do. A run that is
// being driven into runaway or abusive behaviour (calling tools far too fast,
// repeating one action, failing repeatedly, or spending past a hard ceiling) is
// halted by a breaker that the run itself cannot reach or talk its way past.
//
// The brakes are deliberately not a tool and not part of the conversation: the
// agent has no action that disables them, because they live in the trusted core
// and are wired in by the host, not exposed through the tool surface. So a
// jailbroken, injected, or misaligned run is subject to the same brakes as a
// well-behaved one. The kill-switch and the automatic breakers share one halt
// state per run: once it is engaged, every subsequent action that run dispatches
// is refused until it is reset.
//
// This composes with, and does not replace, the other waist hooks. The capability
// admitter decides what a run may do; the budget caps total spend; the brakes
// catch behaviour, the shape of the activity over time, that those static gates do
// not see. A run with no brakes wired is unbraked, which keeps the standalone
// agent zero-config; wiring a Hook turns the safety loop on.
package brakes

import (
	"context"
	"sync"
	"time"
)

// Limits configures the automatic breakers. A zero field disables that breaker,
// so the zero Limits trips nothing and only the kill-switch and any anomaly
// detector are active. Set the axes a run should be braked on and leave the rest
// zero.
type Limits struct {
	// MaxActions is the most actions a run may dispatch within Window before the
	// rate breaker halts it. Zero disables the rate breaker. Window must be set
	// with it; a MaxActions with a zero Window is treated as disabled.
	MaxActions int
	// Window is the trailing span the rate breaker counts actions over.
	Window time.Duration

	// MaxRepeats is the most times one action name may be dispatched in a row
	// before the repeat breaker halts the run. It catches a loop hammering a single
	// tool. Zero disables it.
	MaxRepeats int

	// ErrorRate is the fraction of recent actions that may fail, in [0,1], before
	// the error breaker halts the run. It is evaluated only once at least
	// ErrorRateMinSamples actions have been observed, so an early failure does not
	// trip it. Zero disables it.
	ErrorRate float64
	// ErrorRateMinSamples is the smallest number of observed outcomes before the
	// error breaker may trip, and the window over which the rate is measured. A
	// zero with a non-zero ErrorRate defaults to defaultErrorSamples.
	ErrorRateMinSamples int

	// MaxTokens halts the run once its cumulative metered tokens reach this
	// ceiling. It is a hard behavioural backstop distinct from the budget, which
	// rejects a single over-ceiling action; this stops the whole run. Zero disables it.
	MaxTokens int64
	// MaxCost halts the run once its cumulative metered cost reaches this ceiling.
	// Zero disables it.
	MaxCost float64
}

// defaultErrorSamples is the minimum sample size the error breaker uses when
// ErrorRate is set without an explicit ErrorRateMinSamples, so a couple of early
// failures cannot trip it.
const defaultErrorSamples = 5

// active reports whether any breaker is configured. A Limits with nothing set
// leaves the breakers idle (only the kill-switch and anomaly detector act).
func (l Limits) active() bool {
	return (l.MaxActions > 0 && l.Window > 0) ||
		l.MaxRepeats > 0 ||
		l.ErrorRate > 0 ||
		l.MaxTokens > 0 ||
		l.MaxCost > 0
}

// Switch is the kill-switch: the per-run halt state the brakes enforce. Engage
// trips it (operator-invoked, or auto-tripped by a breaker), Engaged reports
// whether a run is halted and why, and Reset clears it. An implementation must be
// safe for concurrent use. The default MemSwitch keeps the state in memory;
// a host that needs a halt to survive a restart or to propagate across a fleet
// supplies its own durable implementation.
type Switch interface {
	// Engage halts run, recording reason. Engaging an already-halted run keeps the
	// first reason, so the original cause is not overwritten by a later one.
	Engage(run, reason string)
	// Engaged reports whether run is halted and the reason it was halted.
	Engaged(run string) (bool, string)
	// Reset clears the halt on run, so it may dispatch again. It is the deliberate
	// operator action to resume a halted run.
	Reset(run string)
}

// MemSwitch is the default in-memory Switch, safe for concurrent use. A halt it
// records lasts for the life of the process; it does not survive a restart.
type MemSwitch struct {
	mu      sync.Mutex
	reasons map[string]string
}

// NewMemSwitch returns an empty in-memory kill-switch.
func NewMemSwitch() *MemSwitch { return &MemSwitch{reasons: map[string]string{}} }

// Engage halts run, keeping the first reason if it is already halted.
func (s *MemSwitch) Engage(run, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reasons[run]; ok {
		return
	}
	s.reasons[run] = reason
}

// Engaged reports whether run is halted and why.
func (s *MemSwitch) Engaged(run string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reasons[run]
	return ok, r
}

// Reset clears the halt on run.
func (s *MemSwitch) Reset(run string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reasons, run)
}

var _ Switch = (*MemSwitch)(nil)

// AnomalyDetector is the pluggable behavioural-anomaly port. After each action,
// the Hook offers the observed call to the detector, which may halt the run by
// returning true with a reason. It is how an out-of-band signal source (a syscall
// or audit feed that models a run's normal behaviour and flags the out-of-pattern
// case) drives the brakes without the brakes depending on that source. The default
// is no detector; the configured breakers carry the in-process detection.
type AnomalyDetector interface {
	// Observe is offered every completed action with its metering and outcome. It
	// returns true and a reason to halt the run, false to let it continue. It must
	// be safe for concurrent use and must not block.
	Observe(ctx context.Context, action string, tokens int64, cost float64, failed bool) (halt bool, reason string)
}

type ctxKey struct{}

// Into returns a context carrying the run id the brakes track behaviour against,
// so the Hook reads the run from the context rather than from a parameter. Binding
// it once at the top of a run scopes the brakes to that run; a fan-out of runs
// sharing one id shares one set of brakes. An unbound context falls back to a
// single shared bucket, so the safety loop still acts when no id is set rather
// than silently disabling itself.
func Into(ctx context.Context, run string) context.Context {
	return context.WithValue(ctx, ctxKey{}, run)
}

// runFromContext returns the run id bound to ctx, or "" for the shared fallback
// bucket. The brakes are protective by default, so an absent id is tracked, not
// exempted.
func runFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKey{}).(string); ok {
		return id
	}
	return ""
}
