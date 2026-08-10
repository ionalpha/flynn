package goal

// The knobs a composition turns when it assembles a Reconciler. Each option is
// tolerant of a zero or nil argument, so a host that has not wired an optional
// collaborator gets the behaviour it had before the option existed.

import (
	"time"

	"github.com/ionalpha/flynn/bus"
)

// Option configures a Reconciler.
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

// WithWaitRecheck overrides how long a parked goal (waiting on a fan-out's
// children) may sit before the reconciler re-checks it without a wake signal.
func WithWaitRecheck(d time.Duration) Option {
	return func(g *Reconciler) {
		if d > 0 {
			g.waitRecheck = d
		}
	}
}

// WithPlanning makes the goal plan before it builds: the first thing a goal does is
// a planning step that expands its objective into a ledger, and no build step is
// dispatched until that ledger exists. It pairs with a Worker configured with a
// Planner, which is what actually runs the planning step.
//
// It is an option rather than the unconditional behaviour because a goal composed
// without a planner has no way to produce a ledger, and gating those goals would
// park every one of them forever. Wiring the planner and turning this on are the
// same decision, made in the same place.
func WithPlanning() Option { return func(g *Reconciler) { g.planning = true } }

// WithLedgerGate runs the ledger loop: after each build step the run's current item has
// its declared check run, and the verdicts recorded on the run's durable record are folded
// back through gate to settle the ledger. Items flip to proven from the record, and only
// from the record.
//
// Until this is wired the ledger is planned, validated and protected against tampering,
// and then never asked whether the work is actually done, which is the exact failure the
// ledger exists to foreclose, a run declaring victory having written nothing. gate is the
// self-tested EvidenceGate; a nil gate or nil evidence leaves the loop open, so a goal
// behaves exactly as it did before, and pairing them is the composition's job (see
// runtime.Config).
//
// On its own this changes what the record says, not what the goal does: convergence is
// still the model's call. WithLedgerConvergence is what makes the record binding.
func WithLedgerGate(e Evidence, gate *EvidenceGate) Option {
	return func(g *Reconciler) {
		if e != nil && gate != nil {
			g.evidence, g.gate = e, gate
		}
	}
}

// WithLedgerConvergence makes the ledger, not the final answer, decide whether a planned
// goal is done: a model reporting completion with items still unproven does not converge,
// it settles as stalled naming each unproven item and why.
//
// It is separate from WithLedgerGate because the two carry different risk, and the
// staging between them is the point. Turning the refusal on before anything produces
// verifications stalls every goal, which is why the gate's original deferral was correct;
// running the producer first makes items visibly flip to proven on real runs, and this is
// then turned on against evidence rather than hope.
//
// It is emphatically not a switch that may ship permanently off. A gate that is loaded and
// does nothing is precisely the failure the gate's own self-test exists to catch, worn one
// level up, and a composition that leaves this off indefinitely has rebuilt it.
func WithLedgerConvergence() Option { return func(g *Reconciler) { g.ledgerConverge = true } }

// WithWakeBus sets the bus the reconciler signals a parked owner on when one of
// its children settles, so a fan-out parent re-checks on child state-change
// instead of waiting out the recheck fallback.
func WithWakeBus(b bus.Bus) Option { return func(g *Reconciler) { g.bus = b } }

// WithProgressProbe turns on no-progress detection: after each build step the
// reconciler asks the probe for a fingerprint of the substantive work recorded so far,
// and stops the goal once that fingerprint has not changed for NoProgressLimit
// consecutive steps (see progress.go). It is an option rather than always-on because
// the probe reads whatever durable record the runtime keeps (the spine), which a bare
// reconciler assembled without one does not have; a nil probe leaves detection off and
// a goal is bounded only by its step budget, exactly as before.
func WithProgressProbe(p ProgressProbe) Option { return func(g *Reconciler) { g.progress = p } }

// WithWindowSource wires the source the spend guard reads to enforce a goal's
// WindowFraction ceiling (share of the plan window). Flynn ships no source of its own
// because the plan window belongs to the account the host app runs under; without one
// the token and cost ceilings still apply and only the window axis is left unbounded.
func WithWindowSource(w WindowSource) Option { return func(g *Reconciler) { g.window = w } }

// WithStepMaxAttempts bounds how many times a single dispatched step is retried by
// the job queue before it goes dead and stalls the goal (0 uses the queue default).
func WithStepMaxAttempts(n int) Option {
	return func(g *Reconciler) {
		if n > 0 {
			g.stepTries = n
		}
	}
}
