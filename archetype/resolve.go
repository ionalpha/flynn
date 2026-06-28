package archetype

import (
	"context"
	"sort"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// Resolved is an Agent flattened to its effective bundle: the Agent's own fields
// composed with every base it extends. It is what a run is actually configured
// from, so the runtime, the spawner, and the router all consume Resolved rather
// than reaching into the composition chain themselves.
type Resolved struct {
	System       string
	Capabilities []string
	Model        string
	Driver       string
	SkillScope   string
	MemoryScope  string
	Tools        []string
	Knowledge    []string
}

// Grant builds the capability grant for a resolved Agent: the run is admitted only
// for the capabilities the effective bundle declares.
func (r Resolved) Grant() capability.Grant {
	return capability.NewGrant(r.Capabilities...)
}

// Resolve loads the Agent named name from store and flattens its composition chain
// into the effective bundle. Bases are merged in declared order, then the Agent
// itself on top: scalar fields take the most-derived non-empty value (the Agent
// overrides its bases; a later base overrides an earlier one), and the set fields
// (capabilities, tools, knowledge) are the union. A composition cycle is a terminal
// error; a diamond (two bases sharing an ancestor) is allowed. A flat Agent that
// extends nothing resolves to itself.
func Resolve(ctx context.Context, store resource.Store, scope resource.Scope, name string) (Resolved, error) {
	return resolve(ctx, store, scope, name, map[string]bool{})
}

// Flatten flattens a spec already in hand against store (resolving its Extends
// chain), without a name of its own. Use it when the Agent spec is already loaded.
func Flatten(ctx context.Context, store resource.Store, scope resource.Scope, spec Spec) (Resolved, error) {
	return flatten(ctx, store, scope, spec, map[string]bool{})
}

// resolve flattens the Agent named name, guarding against cycles via the active
// path (a name already on the path being resolved again is a back-edge).
func resolve(ctx context.Context, store resource.Store, scope resource.Scope, name string, path map[string]bool) (Resolved, error) {
	if path[name] {
		return Resolved{}, fault.New(fault.Terminal, "agent_extends_cycle",
			"archetype: composition cycle through agent "+name)
	}
	r, err := store.Get(ctx, Kind, scope, name)
	if err != nil {
		return Resolved{}, fault.Wrap(fault.Terminal, "agent_resolve_get", err)
	}
	spec, err := DecodeSpec(r)
	if err != nil {
		return Resolved{}, fault.Wrap(fault.Terminal, "agent_resolve_decode", err)
	}
	path[name] = true
	defer delete(path, name)
	return flatten(ctx, store, scope, spec, path)
}

// flatten merges the spec's bases (in order) then the spec itself.
func flatten(ctx context.Context, store resource.Store, scope resource.Scope, spec Spec, path map[string]bool) (Resolved, error) {
	var acc Resolved
	for _, base := range spec.Extends {
		b, err := resolve(ctx, store, scope, base, path)
		if err != nil {
			return Resolved{}, err
		}
		acc = mergeResolved(acc, b)
	}
	return mergeSpec(acc, spec), nil
}

// mergeResolved overlays b onto acc: a non-empty scalar in b wins, and the set
// fields union. Used to fold each base into the accumulator in order.
func mergeResolved(acc, b Resolved) Resolved {
	return Resolved{
		System:       override(acc.System, b.System),
		Model:        override(acc.Model, b.Model),
		Driver:       override(acc.Driver, b.Driver),
		SkillScope:   override(acc.SkillScope, b.SkillScope),
		MemoryScope:  override(acc.MemoryScope, b.MemoryScope),
		Capabilities: union(acc.Capabilities, b.Capabilities),
		Tools:        union(acc.Tools, b.Tools),
		Knowledge:    union(acc.Knowledge, b.Knowledge),
	}
}

// mergeSpec overlays an Agent's own fields onto acc (the merged bases), so the
// Agent overrides everything it extends.
func mergeSpec(acc Resolved, spec Spec) Resolved {
	return Resolved{
		System:       override(acc.System, spec.System),
		Model:        override(acc.Model, spec.Model),
		Driver:       override(acc.Driver, spec.Driver),
		SkillScope:   override(acc.SkillScope, spec.SkillScope),
		MemoryScope:  override(acc.MemoryScope, spec.MemoryScope),
		Capabilities: union(acc.Capabilities, spec.Capabilities),
		Tools:        union(acc.Tools, spec.Tools),
		Knowledge:    union(acc.Knowledge, spec.Knowledge),
	}
}

// override returns next when it is set, else keeps cur, so the most-derived
// non-empty value wins for a scalar field.
func override(cur, next string) string {
	if next != "" {
		return next
	}
	return cur
}

// union returns the sorted, de-duplicated union of two action/ref sets, so a
// composed bundle is deterministic regardless of merge order.
func union(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(a)+len(b))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, x := range b {
		set[x] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
