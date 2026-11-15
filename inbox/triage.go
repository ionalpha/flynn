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

const defaultTriagePoll = 200 * time.Millisecond

// replyFailureText is sent when a Reply entry's work fails, so the user is not left
// waiting.
const replyFailureText = "Sorry, I could not complete that request."

// Worker runs the work for a Reply or Goal entry, so triage does not depend on the
// goal engine. Start receives the entry id so it can be made idempotent: triage
// may call it again for the same entry when recording the handle fails.
type Worker interface {
	Start(ctx context.Context, entry, objective string) (handle string, err error)
	Poll(ctx context.Context, handle string) (done bool, answer string, failed bool, err error)
}

// Policy chooses a disposition from the entry's content alone. A host supplies one
// to add routing, allow-lists or autonomy rules.
type Policy func(Spec) Disposition

// DefaultPolicy replies to an entry that names a conversation and drops the rest.
func DefaultPolicy(s Spec) Disposition {
	if s.Conversation != "" {
		return DispositionReply
	}
	return DispositionDrop
}

// Sinks routes a reply to the Sink registered for an entry's source.
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

// Send errors when no sink is registered for source, so a lost reply is reported.
func (s *Sinks) Send(ctx context.Context, source, conversation, text string) error {
	sink, ok := s.byName[source]
	if !ok {
		return fmt.Errorf("inbox: no sink registered for source %q", source)
	}
	return sink.Send(ctx, conversation, text)
}

// Triage reconciles entries. Each call re-reads the entry and advances it one
// step. The [Worker] runs the work, so a long task does not block the reconcile
// loop.
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

// WithPolicy sets the disposition policy (default [DefaultPolicy]).
func WithPolicy(p Policy) TriageOption {
	return func(t *Triage) {
		if p != nil {
			t.policy = p
		}
	}
}

// WithPollInterval sets how often in-flight work is re-checked (default 200ms).
// A non-positive value is ignored.
func WithPollInterval(d time.Duration) TriageOption {
	return func(t *Triage) {
		if d > 0 {
			t.poll = d
		}
	}
}

// NewTriage returns a Triage over the entries in store.
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

// Reconcile advances one entry by one phase.
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

func (t *Triage) triage(ctx context.Context, r resource.Resource, spec Spec, st Status) (reconcile.Result, error) {
	now := t.clk.Now()
	disp := t.policy(spec)
	if disp != DispositionReply && disp != DispositionGoal {
		st.Disposition = DispositionDrop
		st.Phase = PhaseDropped
		st.SetCondition(Condition{Type: CondTriaged, Status: "True", Reason: "Drop"}, now)
		return reconcile.Result{}, t.persist(ctx, r, st)
	}

	// Start before recording the handle: if the write fails, the retry starts the
	// work again (at-least-once), and the Worker dedupes by entry id.
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

func (t *Triage) act(ctx context.Context, r resource.Resource, spec Spec, st Status) (reconcile.Result, error) {
	if st.GoalName == "" {
		return reconcile.Result{}, errors.New("inbox: triaged entry has no work handle")
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
	// A Goal sends nothing. The reply is at-least-once: a send error leaves the
	// entry un-acted, and the retry may resend on a flaky sink.
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

func (t *Triage) persist(ctx context.Context, r resource.Resource, st Status) error {
	raw, err := st.Encode()
	if err != nil {
		return err
	}
	r.Status = raw
	_, err = t.store.Put(ctx, r)
	return err
}

var _ reconcile.Reconciler[reconcile.Ref] = (*Triage)(nil)
