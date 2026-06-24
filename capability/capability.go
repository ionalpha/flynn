// Package capability is the agent's least-privilege model: a Grant names exactly
// what a run is permitted to do, and an Admitter enforces it at the dispatch waist
// so an action outside the grant is denied before any side effect. It is the data
// half of governance, the policy the waist's Admitter consults; the sandbox is
// where an admitted action then runs under real OS-level confinement.
//
// The grant is carried on the context, like the observability bundle, so a run
// binds its policy once and every action it dispatches is checked against the same
// grant without threading it through call signatures. A context with no grant
// bound is permissive, which keeps the standalone agent zero-config; a host opts
// into least privilege by binding a grant, and from then on the posture is
// default-deny: only the listed actions are admitted.
package capability

import (
	"context"
	"sort"
)

// Grant is the set of actions a run may perform, addressed by action name (the
// dispatch action a tool resolves to). The zero Grant denies everything; a Grant
// from NewGrant denies everything except the names it lists; AllowAll admits any
// action, the explicit "trusted run" policy distinct from no policy at all. A
// Grant is immutable once built and safe to share across goroutines.
//
// Nothing is admitted implicitly: calling the model is an action like any other
// (mission.ActionModelGenerate), so a least-privilege grant lists it explicitly
// and the grant stays the complete, auditable record of what a run may do. A run
// that should not call the model simply does not grant the action.
type Grant struct {
	actions  map[string]struct{}
	allowAll bool
}

// NewGrant builds a default-deny grant that admits exactly the named actions.
// Duplicates and empty names are ignored.
func NewGrant(actions ...string) Grant {
	set := make(map[string]struct{}, len(actions))
	for _, a := range actions {
		if a != "" {
			set[a] = struct{}{}
		}
	}
	return Grant{actions: set}
}

// AllowAll returns a grant that admits every action. It is the explicit
// trusted-run policy: bind it when a run is fully privileged, as distinct from
// binding no grant at all (which is also permissive but signals "no policy set").
func AllowAll() Grant { return Grant{allowAll: true} }

// Allows reports whether the grant permits the named action.
func (g Grant) Allows(action string) bool {
	if g.allowAll {
		return true
	}
	_, ok := g.actions[action]
	return ok
}

// Actions returns the granted action names in sorted order (empty when the grant
// is AllowAll or deny-all), for audit and introspection.
func (g Grant) Actions() []string {
	out := make([]string, 0, len(g.actions))
	for a := range g.actions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Unrestricted reports whether the grant admits every action (AllowAll).
func (g Grant) Unrestricted() bool { return g.allowAll }

type ctxKey struct{}

// Into returns a context carrying g, so the dispatch waist's Admitter reads the
// run's policy from the context rather than from a parameter. Binding it once at
// the top of a run applies it to every action that run dispatches.
func Into(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, ctxKey{}, g)
}

// FromContext returns the grant bound to ctx and whether one was present. Absent a
// grant the caller should treat the run as unconstrained (no policy set), which is
// what the Admitter does.
func FromContext(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(ctxKey{}).(Grant)
	return g, ok
}
