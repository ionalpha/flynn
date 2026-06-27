package approval

import (
	"context"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// Policy decides how many independent approver signatures an action requires
// before the waist admits it. Zero means the action needs no approval and runs
// under the capability grant alone; a positive number is the quorum (1 for a
// single approver, more for a high-risk M-of-N action). Keeping the requirement as
// data lets a host raise the bar on a destructive action without touching the gate.
type Policy interface {
	Required(a dispatch.Action) int
}

// Requirements is a map-based Policy from action name to required signature count.
// An action not in the map requires no approval, so the default is permissive and a
// host opts specific privileged actions into the gate.
type Requirements map[string]int

// Required returns the signatures the action needs, or zero when it is not listed.
func (r Requirements) Required(a dispatch.Action) int { return r[a.Name] }

var _ Policy = Requirements(nil)

// Sink records an authorization Decision, so a grant or a denial lands on the event
// spine as an immutable, after-the-fact-verifiable record of who authorized what.
// The default DiscardSink drops it, keeping standalone use zero-config; a host
// wires this to the spine.
type Sink interface {
	Record(ctx context.Context, d Decision) error
}

// DiscardSink is the default Sink that records nothing.
type DiscardSink struct{}

// Record implements Sink.
func (DiscardSink) Record(context.Context, Decision) error { return nil }

var _ Sink = DiscardSink{}

type approvalsKey struct{}

// Into returns a context carrying approvals presented for the run's privileged
// actions, accumulating with any already bound, so a quorum can be assembled from
// approvals that arrive separately. The gate reads them from the context, so a
// caller binds them once rather than threading them through every call.
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

// WithDetail binds a target descriptor for the next privileged action, so an
// approval can be narrowed to a specific target (a resource id, an environment)
// rather than the action name alone. The gate folds it into the envelope it
// requires, and an approver must have signed for the same detail. Empty (the
// default) binds by action and scope only.
func WithDetail(ctx context.Context, detail string) context.Context {
	return context.WithValue(ctx, detailKey{}, detail)
}

// detailFromContext returns the bound target descriptor, or "".
func detailFromContext(ctx context.Context) string {
	d, _ := ctx.Value(detailKey{}).(string)
	return d
}

// Binding builds the envelope a privileged action requires, from the action, the
// run's principal and target on the context, and the gate's host. An approver
// signs this envelope (after setting a nonce and validity window) so the signature
// is over exactly what the gate checks; the gate builds the same binding to verify.
func Binding(ctx context.Context, a dispatch.Action, host string) Envelope {
	return Envelope{
		Action:    a.Name,
		Scope:     a.Scope,
		Principal: capability.PrincipalFromContext(ctx),
		Detail:    detailFromContext(ctx),
		Host:      host,
	}
}

// Gate is the approval enforcement at the dispatch waist: a dispatch.Hook whose
// Before refuses a privileged action unless a sufficient quorum of valid approvals
// is presented for it. Because the waist governs every action, one Gate enforces
// approval across the whole run with no per-call wiring. It composes with, and sits
// above, the capability admitter: capability decides the action is allowed in
// principle, approval requires a fresh human (or policy, or peer) authorization for
// this specific privileged instance.
type Gate struct {
	policy   Policy
	verifier *Verifier
	sink     Sink
	host     string
}

// GateOption configures a Gate.
type GateOption func(*Gate)

// WithSink wires the audit sink decisions are recorded to (default: DiscardSink).
func WithSink(s Sink) GateOption {
	return func(g *Gate) {
		if s != nil {
			g.sink = s
		}
	}
}

// WithGateHost sets the host id the gate stamps on the envelope it requires, so an
// approval must be valid for this host. It should match the verifier's host.
func WithGateHost(host string) GateOption {
	return func(g *Gate) { g.host = host }
}

// NewGate builds an approval gate over a policy and verifier. Add it to a
// dispatcher with dispatch.WithHook so every privileged action it governs requires
// approval.
func NewGate(policy Policy, verifier *Verifier, opts ...GateOption) *Gate {
	g := &Gate{policy: policy, verifier: verifier, sink: DiscardSink{}}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Before refuses a privileged action that lacks a sufficient quorum of valid
// approvals. A non-privileged action (policy requirement zero) is admitted
// untouched. The decision, grant or denial, is recorded to the sink before the
// result is returned, so the audit trail captures refused attempts too. A denial
// with no approvals presented is NeedsApproval (the run should pause and request
// one); a denial with approvals that did not verify is Forbidden (it must not be
// retried as-is).
func (g *Gate) Before(ctx context.Context, a dispatch.Action) error {
	required := g.policy.Required(a)
	if required <= 0 {
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

// After is a no-op: approval is decided before the action runs.
func (g *Gate) After(context.Context, dispatch.Action, dispatch.Metering, error) {}

var _ dispatch.Hook = (*Gate)(nil)
