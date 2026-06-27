package inbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/reconcile"
	"github.com/ionalpha/flynn/resource"
)

// defaultTriagePoll is how often triage re-checks in-flight work for a signal it
// is replying to.
const defaultTriagePoll = 200 * time.Millisecond

// replyFailureText is sent when the work for a Reply signal fails, so the user is
// told rather than left waiting.
const replyFailureText = "Sorry, I could not complete that request."

// Worker carries out the action a Reply or Goal disposition implies: it starts work
// for a signal and reports the outcome. Triage depends on this rather than the goal
// engine, so the inbound boundary stays independent of the execution model. The
// signal id is passed so an implementation can make Start idempotent per signal.
type Worker interface {
	Start(ctx context.Context, signal, objective string) (handle string, err error)
	Poll(ctx context.Context, handle string) (done bool, answer string, failed bool, err error)
}

// Policy chooses what to do with a signal from its content alone. The default
// replies to any conversational signal and drops the rest. A host can supply its
// own to add routing, allow-lists, or autonomy rules.
type Policy func(Spec) Disposition

// DefaultPolicy replies to a signal that names a conversation and drops one that
// does not (a signal with nowhere to answer is not actioned by default).
func DefaultPolicy(s Spec) Disposition {
	if s.Conversation != "" {
		return DispositionReply
	}
	return DispositionDrop
}

// Sinks routes a reply to the Sink registered for a signal's source.
type Sinks struct {
	byName map[string]Sink
}

// NewSinks indexes sinks by their Name.
func NewSinks(sinks ...Sink) *Sinks {
	m := make(map[string]Sink, len(sinks))
	for _, s := range sinks {
		m[s.Name()] = s
	}
	return &Sinks{byName: m}
}

// Send delivers text on the sink registered for source. It errors when no sink is
// registered for the source, so a dropped reply is surfaced rather than silent.
func (s *Sinks) Send(ctx context.Context, source, conversation, text string) error {
	sink, ok := s.byName[source]
	if !ok {
		return fmt.Errorf("inbox: no sink registered for source %q", source)
	}
	return sink.Send(ctx, conversation, text)
}

// Triage is the reconciler that turns a recorded Signal into an action. It is
// level-triggered: each call re-reads the live signal and advances it one step,
// from received to a disposition, then (for a reply) waits on the work and sends
// the answer back. It never runs the work itself; that is the Worker's job, so a
// long task does not block the reconcile loop.
type Triage struct {
	store  resource.Store
	worker Worker
	sinks  *Sinks
	policy Policy
	clk    clock.Timing
	poll   time.Duration
}

// TriageOption configures a Triage.
type TriageOption func(*Triage)

// WithPolicy sets the disposition policy (default DefaultPolicy).
func WithPolicy(p Policy) TriageOption {
	return func(t *Triage) {
		if p != nil {
			t.policy = p
		}
	}
}

// WithPollInterval sets how often in-flight work is re-checked (default
// defaultTriagePoll). A non-positive value is ignored.
func WithPollInterval(d time.Duration) TriageOption {
	return func(t *Triage) {
		if d > 0 {
			t.poll = d
		}
	}
}

// NewTriage builds the triage reconciler over store, dispatching Reply/Goal work to
// worker and routing replies through sinks.
func NewTriage(store resource.Store, worker Worker, sinks *Sinks, clk clock.Timing, opts ...TriageOption) *Triage {
	t := &Triage{
		store:  store,
		worker: worker,
		sinks:  sinks,
		policy: DefaultPolicy,
		clk:    clk,
		poll:   defaultTriagePoll,
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Reconcile advances one signal. Only Reply, Goal, and Drop are acted on today;
// Store and Notify are reserved and currently treated as Drop.
func (t *Triage) Reconcile(ctx context.Context, ref reconcile.Ref) (reconcile.Result, error) {
	r, err := t.store.Get(ctx, ref.Kind, ref.Scope, ref.Name)
	if errors.Is(err, resource.ErrNotFound) {
		return reconcile.Result{}, nil // already gone
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	spec, err := DecodeSpec(r)
	if err != nil {
		return reconcile.Result{}, err
	}
	st, err := DecodeStatus(r)
	if err != nil {
		return reconcile.Result{}, err
	}

	switch st.Phase {
	case PhaseActed, PhaseDropped:
		return reconcile.Result{}, nil // settled

	case "", PhaseReceived:
		return t.triage(ctx, r, spec, st)

	case PhaseTriaged:
		return t.act(ctx, r, spec, st)

	default:
		return reconcile.Result{}, nil
	}
}

// triage chooses a disposition. A Reply or Goal starts the work and waits; anything
// else is dropped.
func (t *Triage) triage(ctx context.Context, r resource.Resource, spec Spec, st Status) (reconcile.Result, error) {
	now := t.clk.Now()
	disp := t.policy(spec)
	if disp != DispositionReply && disp != DispositionGoal {
		st.Disposition = DispositionDrop
		st.Phase = PhaseDropped
		st.SetCondition(Condition{Type: CondTriaged, Status: "True", Reason: "Drop"}, now)
		return reconcile.Result{}, t.persist(ctx, r, st)
	}

	// Start the work, then record the handle. If recording fails the work may be
	// started again on retry (at-least-once); the Worker is given the signal id so
	// it can dedupe when that matters.
	handle, err := t.worker.Start(ctx, r.Name, spec.Content)
	if err != nil {
		return reconcile.Result{}, err
	}
	st.Disposition = disp
	st.GoalName = handle
	st.Phase = PhaseTriaged
	st.SetCondition(Condition{Type: CondTriaged, Status: "True", Reason: string(disp)}, now)
	if err := t.persist(ctx, r, st); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: t.poll}, nil
}

// act polls the in-flight work and, once done, replies (for a Reply disposition)
// and marks the signal acted.
func (t *Triage) act(ctx context.Context, r resource.Resource, spec Spec, st Status) (reconcile.Result, error) {
	if st.GoalName == "" {
		return reconcile.Result{}, errors.New("inbox: triaged signal has no work handle")
	}
	done, answer, failed, err := t.worker.Poll(ctx, st.GoalName)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !done {
		return reconcile.Result{RequeueAfter: t.poll}, nil
	}

	text := answer
	if failed {
		text = replyFailureText
	}
	// A Reply sends the answer back on the originating conversation. A Goal is
	// fire-and-forget. The send is at-least-once: on a send error the signal is left
	// un-acted so the next reconcile retries, which may resend on a flaky sink.
	if st.Disposition == DispositionReply && text != "" {
		if err := t.sinks.Send(ctx, spec.Source, spec.Conversation, text); err != nil {
			return reconcile.Result{}, err
		}
	}

	now := t.clk.Now()
	st.Phase = PhaseActed
	st.SetCondition(Condition{Type: CondActed, Status: "True"}, now)
	if failed {
		st.SetCondition(Condition{Type: CondFailed, Status: "True"}, now)
		st.Message = "work failed"
	}
	return reconcile.Result{}, t.persist(ctx, r, st)
}

// persist writes the updated status back onto the signal resource.
func (t *Triage) persist(ctx context.Context, r resource.Resource, st Status) error {
	raw, err := st.Encode()
	if err != nil {
		return err
	}
	r.Status = raw
	_, err = t.store.Put(ctx, r)
	return err
}

// guard: Triage is a reconciler over resource keys.
var _ reconcile.Reconciler[reconcile.Ref] = (*Triage)(nil)
