// Package capability is the agent's least-privilege model: a [Grant] names what a
// run may do, and an [Admitter] at the dispatch waist denies anything else before
// it has a side effect. The sandbox confines what is admitted.
//
// The grant travels on the context, so a run binds it once and every action it
// dispatches is checked against it. A context with no grant is permissive, which
// keeps the standalone agent zero-config; binding a grant makes the run
// default-deny.
package capability

import (
	"context"
	"sort"
)

// Grant is the set of action names a run may dispatch. The zero Grant denies
// everything, [NewGrant] admits only the names it lists, and [AllowAll] admits any
// action. A Grant is immutable and safe to share across goroutines.
//
// Calling the model is an action too (mission.ActionModelGenerate), so a
// least-privilege grant lists it and stays the complete record of what a run may
// do.
type Grant struct {
	actions  map[string]struct{}
	allowAll bool
}

// NewGrant builds a default-deny grant admitting exactly the named actions.
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

// AllowAll returns a grant admitting every action: an explicit trusted-run policy,
// as opposed to binding no grant, which is also permissive but means no policy set.
func AllowAll() Grant { return Grant{allowAll: true} }

// Allows reports whether the grant permits the named action.
func (g Grant) Allows(action string) bool {
	if g.allowAll {
		return true
	}
	_, ok := g.actions[action]
	return ok
}

// Actions returns the granted names, sorted; empty for AllowAll and deny-all.
func (g Grant) Actions() []string {
	out := make([]string, 0, len(g.actions))
	for a := range g.actions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Unrestricted reports whether the grant is AllowAll.
func (g Grant) Unrestricted() bool { return g.allowAll }

// Narrow returns the intersection of g and the requested actions, so a child given
// parent.Narrow(requested...) can never exceed its parent. Narrowing AllowAll
// yields exactly the requested actions, so a trusted parent still hands a child a
// least-privilege grant. No actions, or only denied ones, yield deny-all.
func (g Grant) Narrow(actions ...string) Grant {
	set := make(map[string]struct{}, len(actions))
	for _, a := range actions {
		if a != "" && g.Allows(a) {
			set[a] = struct{}{}
		}
	}
	return Grant{actions: set}
}

// Intersect returns the grant admitting only what both grants allow, so a caller's
// grant against a target's local grant never exceeds either side. AllowAll is the
// identity: AllowAll intersect g is g, and two AllowAll grants yield AllowAll.
// Unlike [Grant.Narrow], an AllowAll operand is kept rather than collapsed to its
// empty action list.
func (g Grant) Intersect(other Grant) Grant {
	switch {
	case g.allowAll && other.allowAll:
		return AllowAll()
	case g.allowAll:
		return other
	case other.allowAll:
		return g
	}
	set := make(map[string]struct{}, len(g.actions))
	for a := range g.actions {
		if _, ok := other.actions[a]; ok {
			set[a] = struct{}{}
		}
	}
	return Grant{actions: set}
}

type ctxKey struct{}

// Into returns a context carrying g, which the dispatch waist's Admitter reads for
// every action the run dispatches.
func Into(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, ctxKey{}, g)
}

// FromContext returns the grant bound to ctx and whether one was present. With none,
// treat the run as unconstrained, as the Admitter does.
func FromContext(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(ctxKey{}).(Grant)
	return g, ok
}

type principalKey struct{}

// WithPrincipal binds the identity a run acts for: which agent in a fan-out, or
// which human in a multi-user host. The actor kind says what sort of thing acted;
// the principal says who. Events the run records carry it for audit. The empty
// principal is the standalone agent.
func WithPrincipal(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, principalKey{}, id)
}

// PrincipalFromContext returns the principal bound to ctx, or "" when none is set.
func PrincipalFromContext(ctx context.Context) string {
	id, _ := ctx.Value(principalKey{}).(string)
	return id
}
