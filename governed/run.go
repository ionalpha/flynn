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

// DefaultStopCondition is what a run means by finished when nobody says otherwise: the
// objective, judged achieved. It is the run's own account of its work, which is why a
// goal's invariants are audited separately and first.
const DefaultStopCondition = "the objective is fully accomplished"

// StopReason says why a run ended.
type StopReason string

// The stop reasons.
const (
	// StopConverged: the stop condition held and every guard that applied was satisfied.
	StopConverged StopReason = "converged"
	// StopStalled: the goal could not continue; Result.Message says why.
	StopStalled StopReason = "stalled"
	// StopInvariantBreached: a term the goal stated stopped being true.
	StopInvariantBreached StopReason = "invariant_breached"
	// StopCancelled: the caller's context ended before the goal settled.
	StopCancelled StopReason = "cancelled"
)

// Result is what a finished run reports. It is populated on a stall or cancellation as
// well, so a caller that gets an error still has the run id to find the record by.
type Result struct {
	// RunID is the session id, goal name and spine stream of the run.
	RunID string
	// Text is the model's final answer. It is empty unless the run converged.
	Text string
	// Stop says why the run ended.
	Stop StopReason
	// Message is the goal's settled message: the final answer on convergence, the reason
	// on a stall.
	Message string
	// Steps is how many reconcile steps the goal spent.
	Steps int
	// Posture is what the run was assembled with, including every waived guard, so a
	// host that only keeps the result can still say how governed the run was.
	Posture Posture
	// Record is the run's events in order. It is read after the run ends and is empty if
	// the log could not be read; the spine remains the authority.
	Record []session.Event
}

// Run is one assembled, governed run. The caller owns it for the length of one goal and
// closes it.
type Run struct {
	rt       *runtime.Runtime
	p        *parts
	store    resource.Store
	log      spine.Log
	posture  Posture
	mode     Mode
	external *ExternalAgent
}

// ID is the run's identity: its session id, its goal's name, its spine stream and its
// budget key.
func (r *Run) ID() string { return r.p.sess.ID() }

// Session is the session that records the run onto the spine, for a caller that streams
// the conversation live.
func (r *Run) Session() *session.Session { return r.p.sess }

// Runtime is the goal runtime. Start it, then submit or resume a goal through
// [Run.Session]; [Run.Do] does all of that.
func (r *Run) Runtime() *runtime.Runtime { return r.rt }

// Posture reports what was wired.
func (r *Run) Posture() Posture { return r.posture }

// Toolset is the tools the run offers.
func (r *Run) Toolset() []mission.Tool { return append([]mission.Tool(nil), r.p.toolset...) }

// Grant is the authority the run's actions are admitted against.
func (r *Run) Grant() capability.Grant { return r.p.grant }

// Sandbox is the sandbox the run's tools act through, or nil for a run that has none (a
// caller-supplied toolset, or an external agent that brings its own). A host that checks a
// run's work, a verifier say, runs the check through this one so it is confined the same
// way the run is.
func (r *Run) Sandbox() sandbox.Sandbox { return r.p.sb }

// Close releases what the run built. A sandbox the host supplied is the host's to close.
func (r *Run) Close() error {
	if r == nil {
		return nil
	}
	return r.p.close()
}

// Prepare returns a context carrying an egress observer bound to the run's stream: every
// dial netguard makes on the run's behalf reports its allow or block verdict into the same
// recorded history as the run's governed actions. Start the runtime with the returned
// context. The spend ceiling does not depend on it; [Assemble] opens that.
func (r *Run) Prepare(ctx context.Context) (context.Context, error) {
	egress := spinesink.NewEgress(r.log, r.ID())
	return netguard.WithObserver(ctx, egress.Observe), nil
}

// GoalSpec fills a spec's mode defaults: the stop condition, the larger step budget a
// fan-out parent spends dispatching and polling, and the external agent's model string.
// A field the caller set is left alone.
func (r *Run) GoalSpec(spec goal.Spec) goal.Spec {
	if strings.TrimSpace(spec.StopCondition) == "" {
		spec.StopCondition = DefaultStopCondition
	}
	if spec.Model == "" && r.external != nil {
		spec.Model = r.external.Model
	}
	// A fan-out parent spends steps a single conversation does not: one dispatching each
	// delegation and one per poll while it waits for the children. The safety brake and
	// the fan-out width still bound a runaway.
	if spec.MaxSteps == 0 && r.mode == ModeFanout {
		spec.MaxSteps = FanoutRootMaxSteps
	}
	return spec
}

// Submit opens the run's session and submits spec as its goal. The runtime must be
// started. A caller that streams the conversation live uses this and [Run.Resume];
// [Run.Do] uses this too.
func (r *Run) Submit(ctx context.Context, spec goal.Spec) (resource.Key, error) {
	return r.p.sess.Submit(ctx, r.rt, r.GoalSpec(spec))
}

// Resume continues the stored run id on the same stream. The runtime must be started. The
// posture this assembly states is the one [Assemble] recorded, which may differ from the
// run being continued.
func (r *Run) Resume(ctx context.Context, id string) error {
	g, err := r.rt.Resume(ctx, id)
	if err != nil {
		return err
	}
	r.p.sess.Resume(ctx, r.rt, g.Key())
	return nil
}

// Do runs spec to the end and reports how it ended. It starts the runtime, submits the
// goal, waits for it to settle and stops the runtime again, so nothing it started outlives
// the call. A stalled or cancelled goal returns the populated [Result] and an error.
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

// settle reads how the goal ended from its status, which the reconciler owns, and the
// record from the log, after the runtime has stopped writing to either.
func (r *Run) settle(ctx context.Context, key resource.Key, text string, werr error) Result {
	res := Result{RunID: r.ID(), Text: text, Stop: StopStalled, Posture: r.posture}
	switch {
	case werr == nil:
		res.Stop = StopConverged
	case errors.Is(werr, context.Canceled), errors.Is(werr, context.DeadlineExceeded):
		res.Stop = StopCancelled
	}
	// The caller's context may be the thing that ended; read with one that cannot be.
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
