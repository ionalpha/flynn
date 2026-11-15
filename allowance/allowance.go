// Package allowance is the pre-declaration gate at the dispatch waist: an action whose
// effects leave the workspace and cannot be undone runs only if the run's author declared
// it in advance.
//
// It guards against an agent that infers from a goal-level instruction that a destructive
// change is wanted and makes it unconfirmed. In goal mode nobody is watching, so a prompt
// goes unanswered, and a refusal handed to the model reads as an obstacle to route around.
// The author states the authority on the goal spec instead, and a run that reaches an
// undeclared action is stopped and handed back with the ask (see goal's allowance pause).
//
// A capability grant says which actions a run may take at all. Approval needs a signed
// authorization for one instance, from an approver reachable during the run. An allowance
// is a standing authorization written before an unattended run. Each of the three refuses
// on its own.
//
// The waist does not read payloads, so the gate keys on the action name, and the host says
// which actions are irreversible ([Actions]). A call site that knows its target binds it
// with [WithTarget] so a declaration can be narrowed to it.
package allowance

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// CodeRequired is the fault code the gate refuses with and the rule name on the run's
// record. The goal reconciler matches it to turn a refusal into the ask for the author.
const CodeRequired = "allowance_required"

// Declaration is one standing authorization: an action the run may take even though it
// reaches outside the workspace irreversibly, optionally narrowed to one target.
//
//flynn:policy
type Declaration struct {
	// Action is the dispatch action name, matched exactly. There is no pattern form,
	// because a declaration covering actions its author did not list is an inference.
	Action string
	// Target narrows the authorization to one target bound with [WithTarget] (a resource
	// id, an environment, a path). Empty covers any target, and is the only form that
	// works for a call site that binds none.
	Target string
}

// Policy says which actions reach outside the workspace irreversibly and so need a
// declaration. The host supplies it because only the host knows what an action does.
//
//flynn:policy TestFailClosedAllowanceGate
type Policy interface {
	Outside(a dispatch.Action) bool
}

// Actions is a [Policy] over action names. An action not in the set needs no
// declaration, so a host opts actions in, as with the approval policy.
type Actions map[string]bool

// NewActions builds the set, dropping blanks so an empty flag value enrols nothing.
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

// Into returns ctx with decls added to any already bound. A run binds its goal's
// allowances once, at the top of its execution.
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

// WithTarget binds the target the next action acts on. Unbound, the gate checks the
// action alone.
func WithTarget(ctx context.Context, target string) context.Context {
	return context.WithValue(ctx, targetKey{}, target)
}

// targetFromContext returns the bound target, or "".
func targetFromContext(ctx context.Context) string {
	t, _ := ctx.Value(targetKey{}).(string)
	return t
}

// Covers reports whether a declaration names action and either target or no target.
// Both comparisons are exact, with no prefix or glob, so one declared string can never
// cover undeclared ones.
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

// Gate is the [dispatch.Hook] that refuses an action the policy marks unless the run
// carries a declaration for it. Unmarked actions pass untouched.
//
//flynn:policy TestFailClosedAllowanceGate
type Gate struct {
	policy Policy
}

// NewGate builds a gate over policy, for [dispatch.WithHook]. A nil policy marks
// nothing.
func NewGate(policy Policy) *Gate { return &Gate{policy: policy} }

// Before refuses an undeclared marked action with fault.NeedsApproval: nobody has said
// the run may, and the author can answer that, so the author sees an ask. Reconcile
// retries neither NeedsApproval nor Forbidden.
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
