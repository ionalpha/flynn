// Package allowance is the pre-declaration gate at the dispatch waist: an action whose
// effects leave the workspace and cannot be undone runs only if the run's author declared
// it in advance. No declaration, no action.
//
// It exists for a specific reported failure: an agent inferred from a goal-level
// instruction that a destructive change to persistent state was wanted, made it with no
// confirmation and no backup, and did so several times in one session. Nothing about that
// is a permission the run was refused. The run was told what to accomplish, worked out
// what accomplishing it needed, and the destructive part was its own reading of the
// instruction. That is the shape the name is about: the authority for an irreversible
// action outside the workspace has to be declared, never inferred from the objective.
//
// The declaration is not a confirmation prompt. Under goal mode the human is by definition
// not watching, so an interactive prompt is a question asked into an empty room, and a
// refusal handed back to the model is worse than that: the model reads it as an obstacle
// and looks for a route around it, which is the failure the refusal was supposed to
// prevent. So the authority is stated up front, on the goal spec, by the person who wrote
// the objective; a run that reaches an undeclared one is stopped and handed back to them
// with the ask (see goal's allowance pause).
//
// How this differs from the two gates it sits beside. A capability grant says which
// actions the run may take at all, and it is checked the same way every time. Approval
// requires a fresh, signed authorization for one privileged instance of an action, which
// needs an approver reachable while the run is going. An allowance is neither: it is a
// standing authorization the author wrote before the run started, for a run they will not
// be watching. All three can apply to one action, and each refuses on its own.
//
// The waist is payload-agnostic, so this gate governs an action's identity rather than its
// arguments: which actions leave the workspace irreversibly is host data (Actions), not
// something inferred from a path or a command line. Where a call site does know the target
// it binds one with WithTarget, and the declaration can then be narrowed to it; where
// nothing binds a target, an action-level declaration is what the gate checks.
package allowance

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// CodeRequired is the fault code the gate refuses with, and the rule name the refusal
// carries on the run's record. The goal reconciler reads the record back for exactly this
// code to turn a refusal into the ask it hands its author, so the two sides agree on one
// constant rather than on two copies of a string.
const CodeRequired = "allowance_required"

// Declaration is one standing authorization: an action the run may take even though it
// reaches outside the workspace irreversibly, optionally narrowed to one target.
type Declaration struct {
	// Action is the dispatch action name being authorized. It is matched exactly: an
	// allowance for one action never authorizes another, and there is no pattern form,
	// because a declaration that covers actions its author did not enumerate is the
	// inference this gate exists to refuse.
	Action string
	// Target narrows the authorization to one target the call site binds with WithTarget
	// (a resource id, an environment, a path). Empty authorizes the action whatever
	// target it is attempted against, which is the widest form a declaration takes and is
	// the only form available for an action whose call site binds no target.
	Target string
}

// Policy decides which actions reach outside the workspace irreversibly and therefore
// need a declaration. It is host data rather than a judgement made at the waist: dispatch
// governs an action's identity and never its payload, so the host that knows what an
// action does is the one that can say whether its effects can be taken back.
type Policy interface {
	Outside(a dispatch.Action) bool
}

// Actions is a set-based Policy over action names. An action not in the set needs no
// declaration, so the default is permissive and a host opts specific actions into the
// gate, the same shape the approval policy takes.
type Actions map[string]bool

// NewActions builds the set from action names, dropping blanks so a stray empty flag
// value cannot enrol an action named the empty string.
func NewActions(names ...string) Actions {
	set := make(Actions, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}

// Outside implements Policy.
func (s Actions) Outside(a dispatch.Action) bool { return s[a.Name] }

var _ Policy = Actions(nil)

type declarationsKey struct{}

// Into returns a context carrying the declarations a run holds, accumulating with any
// already bound. A run binds its goal's allowances once, at the top of its execution, and
// every action it dispatches is checked against them.
func Into(ctx context.Context, decls ...Declaration) context.Context {
	existing, _ := ctx.Value(declarationsKey{}).([]Declaration)
	merged := make([]Declaration, 0, len(existing)+len(decls))
	merged = append(merged, existing...)
	merged = append(merged, decls...)
	return context.WithValue(ctx, declarationsKey{}, merged)
}

// FromContext returns the declarations bound to ctx, or nil when none are.
func FromContext(ctx context.Context) []Declaration {
	d, _ := ctx.Value(declarationsKey{}).([]Declaration)
	return d
}

type targetKey struct{}

// WithTarget binds the target the next action acts on, so a declaration can be narrowed
// to it. A call site that knows what it is about to touch binds it; one that does not
// leaves it unbound, and the gate then checks the action alone.
func WithTarget(ctx context.Context, target string) context.Context {
	return context.WithValue(ctx, targetKey{}, target)
}

// targetFromContext returns the bound target, or "".
func targetFromContext(ctx context.Context) string {
	t, _ := ctx.Value(targetKey{}).(string)
	return t
}

// Covers reports whether decls authorize action against target. A declaration matches when
// it names the same action and either names the same target or names none at all.
//
// The comparison is exact on both halves. Nothing here interprets a target as a path with
// a prefix or a glob: a rule where one declared string covers a family of undeclared ones
// is where an authorization quietly widens, and widening is the whole thing this refuses.
func Covers(decls []Declaration, action, target string) bool {
	action, target = strings.TrimSpace(action), strings.TrimSpace(target)
	if action == "" {
		return false
	}
	for _, d := range decls {
		if strings.TrimSpace(d.Action) != action {
			continue
		}
		if t := strings.TrimSpace(d.Target); t == "" || t == target {
			return true
		}
	}
	return false
}

// Gate is the pre-declaration enforcement at the dispatch waist: a dispatch.Hook whose
// Before refuses an action the policy marks as reaching outside the workspace unless the
// run carries a declaration for it. An action the policy does not mark is admitted
// untouched, so a run whose host declares nothing is unaffected.
type Gate struct {
	policy Policy
}

// NewGate builds a gate over a policy. Add it to a dispatcher with dispatch.WithHook so
// every action it governs needs a declaration. A nil policy marks nothing and the gate
// admits everything, which is the zero-config default.
func NewGate(policy Policy) *Gate { return &Gate{policy: policy} }

// Before refuses an undeclared action that reaches outside the workspace.
//
// The refusal is NeedsApproval rather than Forbidden, and the difference is the point.
// Forbidden says the run may not do this; NeedsApproval says nobody has said it may, which
// is a question with an answer and a person who can give it. The reconcile layer does not
// hot-retry either class, and the run's author sees an ask rather than a verdict.
func (g *Gate) Before(ctx context.Context, a dispatch.Action) error {
	if g.policy == nil || !g.policy.Outside(a) {
		return nil
	}
	target := targetFromContext(ctx)
	if Covers(FromContext(ctx), a.Name, target) {
		return nil
	}
	msg := "action " + a.Name + " reaches outside the workspace and cannot be undone"
	if target != "" {
		msg += " (target " + target + ")"
	}
	return fault.New(fault.NeedsApproval, CodeRequired,
		msg+": it was not declared in advance, and a goal-level instruction is not a declaration")
}

// After is a no-op: a declaration is checked before the action runs.
func (g *Gate) After(context.Context, dispatch.Action, dispatch.Metering, error) {}

var _ dispatch.Hook = (*Gate)(nil)
