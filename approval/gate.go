package approval

import (
	"context"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// Policy gives the number of independent signatures an action needs. Zero
// means the capability grant alone admits it. Keeping the requirement as data
// lets a host raise the bar on a destructive action without touching the gate.
//
//flynn:policy TestFailClosedGate
type Policy interface {
	Required(a dispatch.Action) int
}

// Requirements maps action name to required signature count. An unlisted
// action needs no approval, so a host opts specific actions into the gate.
type Requirements map[string]int

// Required returns the signatures a needs, or zero when it is not listed.
func (r Requirements) Required(a dispatch.Action) int { return r[a.Name] }

var _ Policy = Requirements(nil)

// Sink records each [Decision], grant or denial, so the spine holds a
// verifiable record of who authorized what. A host wires it to the spine;
// [DiscardSink] is the default.
type Sink interface {
	Record(ctx context.Context, d Decision) error
}

// DiscardSink is the default Sink and records nothing.
type DiscardSink struct{}

// Record implements Sink.
func (DiscardSink) Record(context.Context, Decision) error { return nil }

var _ Sink = DiscardSink{}

type approvalsKey struct{}

// Into adds approvals to ctx, keeping any already bound, so a quorum can be
// assembled from approvals that arrive separately.
func Into(ctx context.Context, approvals ...Approval) context.Context {
	existing, _ := ctx.Value(approvalsKey{}).([]Approval)
	merged := make([]Approval, 0, len(existing)+len(approvals))
	merged = append(merged, existing...)
	merged = append(merged, approvals...)
	return context.WithValue(ctx, approvalsKey{}, merged)
}

// FromContext returns the approvals bound to ctx, or nil when none are.
func FromContext(ctx context.Context) []Approval {
	a, _ := ctx.Value(approvalsKey{}).([]Approval)
	return a
}

type detailKey struct{}

// WithDetail binds a target descriptor (a resource id, an environment) for the
// next privileged action. The gate folds it into the envelope it requires, so
// the approver must have signed for the same detail.
func WithDetail(ctx context.Context, detail string) context.Context {
	return context.WithValue(ctx, detailKey{}, detail)
}

func detailFromContext(ctx context.Context) string {
	d, _ := ctx.Value(detailKey{}).(string)
	return d
}

// Binding builds the envelope a privileged action requires from the action,
// the principal and detail on ctx, and host. The approver signs this envelope,
// after setting a nonce and window, and the gate rebuilds it to verify, so the
// signature covers exactly what the gate checks.
func Binding(ctx context.Context, a dispatch.Action, host string) Envelope {
	return Envelope{
		Action:    a.Name,
		Scope:     a.Scope,
		Principal: capability.PrincipalFromContext(ctx),
		Detail:    detailFromContext(ctx),
		Host:      host,
	}
}

// Gate is a dispatch.Hook that refuses a privileged action unless enough valid
// approvals are presented. The waist governs every action, so one Gate covers
// the whole run. It runs after the capability admitter: capability allows the
// action in principle, and approval authorizes this instance of it.
//
//flynn:policy
type Gate struct {
	policy   Policy
	verifier *Verifier
	sink     Sink
	host     string
}

// GateOption configures a Gate.
type GateOption func(*Gate)

// WithSink sets the audit sink. Default: [DiscardSink].
func WithSink(s Sink) GateOption {
	return func(g *Gate) {
		if s != nil {
			g.sink = s
		}
	}
}

// WithGateHost sets the host stamped on the required envelope. It should match
// the verifier's host.
func WithGateHost(host string) GateOption {
	return func(g *Gate) { g.host = host }
}

// NewGate builds a gate over policy and verifier. Add it to a dispatcher with
// dispatch.WithHook.
func NewGate(policy Policy, verifier *Verifier, opts ...GateOption) *Gate {
	g := &Gate{policy: policy, verifier: verifier, sink: DiscardSink{}}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Before admits an action with requirement zero and refuses a negative
// requirement as Forbidden. Otherwise it records the decision to the sink
// before returning, so refused attempts are audited too. A denial with no
// approvals is NeedsApproval (pause and ask); one whose approvals did not
// verify is Forbidden (do not retry as-is).
func (g *Gate) Before(ctx context.Context, a dispatch.Action) error {
	required := g.policy.Required(a)
	if required < 0 {
		return fault.New(fault.Forbidden, "approval_policy",
			"action "+a.Name+" not authorized: the approval policy returned a negative requirement")
	}
	if required == 0 {
		return nil
	}
	want := Binding(ctx, a, g.host)
	presented := FromContext(ctx)

	dec, err := g.verifier.Check(ctx, want, presented, required)
	if err != nil {
		return fault.Wrap(fault.Forbidden, "approval_check", err)
	}
	_ = g.sink.Record(ctx, dec) // best-effort audit; a sink error must not admit the action

	if dec.Granted {
		return nil
	}
	if len(presented) == 0 {
		return fault.New(fault.NeedsApproval, "approval_required",
			"action "+a.Name+" requires authorization: "+dec.Reason)
	}
	return fault.New(fault.Forbidden, "approval_denied",
		"action "+a.Name+" not authorized: "+dec.Reason)
}

// After does nothing: approval is decided before the action runs.
func (g *Gate) After(context.Context, dispatch.Action, dispatch.Metering, error) {}

var _ dispatch.Hook = (*Gate)(nil)
