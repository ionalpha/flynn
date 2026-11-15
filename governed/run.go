package governed

import (
	"context"
	"errors"
	"strings"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
)

// DefaultStopCondition is the run's own judgement of its work, which is why invariants
// are audited separately and first.
const DefaultStopCondition = "the objective is fully accomplished"

// StopReason says why a run ended.
type StopReason string

// The stop reasons.
const (
	// StopConverged: the stop condition held and every applicable guard was satisfied.
	StopConverged StopReason = "converged"
	// StopStalled: the goal could not continue; Result.Message says why.
	StopStalled StopReason = "stalled"
	// StopInvariantBreached: a term the goal stated stopped being true.
	StopInvariantBreached StopReason = "invariant_breached"
	// StopCancelled: the caller's context ended before the goal settled.
	StopCancelled StopReason = "cancelled"
)

// Result is what a finished run reports. It is filled on a stall or cancellation too, so
// a caller with an error still has the run id.
type Result struct {
	// RunID is the session id, goal name and spine stream.
	RunID string
	// Text is the final answer, empty unless the run converged.
	Text string
	Stop StopReason
	// Message is the final answer on convergence, the reason on a stall.
	Message string
	// Steps counts reconcile steps.
	Steps   int
	Posture Posture
	// Record is empty if the log could not be read; the spine stays the authority.
	Record []session.Event
}

// Run is one assembled run. The caller owns it for one goal and closes it.
type Run struct {
	rt       *runtime.Runtime
	p        *parts
	store    resource.Store
	log      spine.Log
	posture  Posture
	mode     Mode
	external *ExternalAgent
}

// ID is the session id, goal name, spine stream and budget key.
func (r *Run) ID() string { return r.p.sess.ID() }

// Session records the run, for a caller that streams it live.
func (r *Run) Session() *session.Session { return r.p.sess }

// Runtime is the goal runtime. [Run.Do] starts it and submits; a caller may do both
// itself.
func (r *Run) Runtime() *runtime.Runtime { return r.rt }

// Posture reports what was wired.
func (r *Run) Posture() Posture { return r.posture }

// Toolset returns a copy of the offered tools.
func (r *Run) Toolset() []mission.Tool { return append([]mission.Tool(nil), r.p.toolset...) }

// Grant admits the run's actions.
func (r *Run) Grant() capability.Grant { return r.p.grant }

// Sandbox is the tools' sandbox, or nil for a toolset or external run. A host check should
// run through it, so it is confined as the run is.
func (r *Run) Sandbox() sandbox.Sandbox { return r.p.sb }

// Close releases what the run built, but not a host-supplied sandbox.
func (r *Run) Close() error {
	if r == nil {
		return nil
	}
	return r.p.close()
}

// Prepare returns ctx with an egress observer, so each netguard verdict is recorded on
// the run's stream. Start the runtime with it.
func (r *Run) Prepare(ctx context.Context) (context.Context, error) {
	egress := spinesink.NewEgress(r.log, r.ID())
	return netguard.WithObserver(ctx, egress.Observe), nil
}

// GoalSpec fills unset fields with the mode's defaults: stop condition, fan-out step
// budget, external model.
func (r *Run) GoalSpec(spec goal.Spec) goal.Spec {
	if strings.TrimSpace(spec.StopCondition) == "" {
		spec.StopCondition = DefaultStopCondition
	}
	if spec.Model == "" && r.external != nil {
		spec.Model = r.external.Model
	}
	// The brake and fan-out width still bound a runaway.
	if spec.MaxSteps == 0 && r.mode == ModeFanout {
		spec.MaxSteps = FanoutRootMaxSteps
	}
	return spec
}

// Submit opens the session and submits spec. The runtime must be started.
func (r *Run) Submit(ctx context.Context, spec goal.Spec) (resource.Key, error) {
	return r.p.sess.Submit(ctx, r.rt, r.GoalSpec(spec))
}

// Resume continues run id on its stream. The runtime must be started. This assembly's
// posture may differ from the original run's.
func (r *Run) Resume(ctx context.Context, id string) error {
	g, err := r.rt.Resume(ctx, id)
	if err != nil {
		return err
	}
	r.p.sess.Resume(ctx, r.rt, g.Key())
	return nil
}

// Do runs spec to the end. Nothing it starts outlives the call. A stall or cancellation
// returns a filled [Result] and an error.
func (r *Run) Do(ctx context.Context, spec goal.Spec) (Result, error) {
	ctx, err := r.Prepare(ctx)
	if err != nil {
		return Result{RunID: r.ID()}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = r.rt.Start(diag.LabelGoroutine(runCtx, "component", "goal-runtime")); close(done) }()

	key, serr := r.Submit(runCtx, spec)
	var text string
	werr := serr
	if serr == nil {
		text, werr = r.p.sess.Wait(runCtx)
	}
	res := r.settle(ctx, key, text, werr)
	cancel()
	<-done
	return res, werr
}

// settle reads status and record after the runtime has stopped writing both.
func (r *Run) settle(ctx context.Context, key resource.Key, text string, werr error) Result {
	res := Result{RunID: r.ID(), Text: text, Stop: StopStalled, Posture: r.posture}
	switch {
	case werr == nil:
		res.Stop = StopConverged
	case errors.Is(werr, context.Canceled), errors.Is(werr, context.DeadlineExceeded):
		res.Stop = StopCancelled
	}
	// The caller's context may have ended.
	rctx := context.WithoutCancel(ctx)
	if key.Name != "" {
		if rec, err := r.store.Get(rctx, key.Kind, key.Scope, key.Name); err == nil {
			if st, derr := goal.DecodeStatus(rec); derr == nil {
				res.Steps, res.Message = st.Steps, st.Message
				if _, breached := st.BreachedInvariant(); breached && res.Stop == StopStalled {
					res.Stop = StopInvariantBreached
				}
			}
		}
	}
	if hist, err := session.History(rctx, r.log, r.ID()); err == nil {
		res.Record = hist
	}
	return res
}
