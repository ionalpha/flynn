package goal

import (
	"time"

	"github.com/ionalpha/flynn/bus"
)

// Option configures a [Reconciler]. A zero or nil argument leaves the feature off.
type Option func(*Reconciler)

// WithCleaner sets the teardown hook run before a goal's finalizer is removed.
func WithCleaner(c Cleaner) Option { return func(g *Reconciler) { g.cleaner = c } }

// WithPollInterval overrides the in-flight re-check interval.
func WithPollInterval(d time.Duration) Option {
	return func(g *Reconciler) {
		if d > 0 {
			g.poll = d
		}
	}
}

// WithWaitRecheck sets how long a parked fan-out goal waits without a wake signal.
func WithWaitRecheck(d time.Duration) Option {
	return func(g *Reconciler) {
		if d > 0 {
			g.waitRecheck = d
		}
	}
}

// WithPlanning holds build steps until a planning step writes the ledger. Turn it on only
// with a Worker that has a Planner, because without one every goal parks forever.
func WithPlanning() Option { return func(g *Reconciler) { g.planning = true } }

// WithLedgerGate runs each item's check after a build step and proves items only from the
// record, through gate. Without it the ledger is never asked whether the work is done. A
// nil argument leaves the loop open. Alone it changes the record and not the outcome;
// [WithLedgerConvergence] makes it binding.
func WithLedgerGate(e Evidence, gate *EvidenceGate) Option {
	return func(g *Reconciler) {
		if e != nil && gate != nil {
			g.evidence, g.gate = e, gate
		}
	}
}

// WithLedgerConvergence stalls a completion claim over unproven items. It is separate from
// [WithLedgerGate] because refusing before anything produces verifications stalls every
// goal. Leaving it off for good makes the gate a no-op.
func WithLedgerConvergence() Option { return func(g *Reconciler) { g.ledgerConverge = true } }

// WithWakeBus wakes a parked parent when a child settles, ahead of the recheck fallback.
func WithWakeBus(b bus.Bus) Option { return func(g *Reconciler) { g.bus = b } }

// WithProgressProbe stops a goal after [NoProgressLimit] idle steps. It is optional
// because the probe needs the runtime's durable record; without it only the step budget
// bounds a goal.
func WithProgressProbe(p ProgressProbe) Option { return func(g *Reconciler) { g.progress = p } }

// WithWindowSource enforces WindowFraction. Flynn ships no source, because the window
// belongs to the host's account.
func WithWindowSource(w WindowSource) Option { return func(g *Reconciler) { g.window = w } }

// WithStepMaxAttempts bounds queue retries of one step before it goes dead and stalls the
// goal. 0 keeps the queue default.
func WithStepMaxAttempts(n int) Option {
	return func(g *Reconciler) {
		if n > 0 {
			g.stepTries = n
		}
	}
}
