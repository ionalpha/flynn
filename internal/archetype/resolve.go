package archetype

import (
	"context"
	"sort"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// Resolved is an Agent flattened with every base it extends, the bundle a run is
// configured from. The runtime, spawner and router read it, so none walks the
// composition chain itself.
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

// Grant builds the capability grant admitting only the bundle's capabilities.
func (r Resolved) Grant() capability.Grant {
	return capability.NewGrant(r.Capabilities...)
}

// Resolve loads the Agent named name and flattens its composition chain. Bases merge
// in declared order with the Agent on top: a scalar takes the most-derived non-empty
// value (a later base overrides an earlier one), and capabilities, tools and
// knowledge are unioned. A cycle is an error; a diamond is allowed.
func Resolve(ctx context.Context, store resource.Store, scope resource.Scope, name string) (Resolved, error) {
	return resolve(ctx, store, scope, name, map[string]bool{})
}

// Flatten resolves the Extends chain of a spec already loaded.
func Flatten(ctx context.Context, store resource.Store, scope resource.Scope, spec Spec) (Resolved, error) {
	return flatten(ctx, store, scope, spec, map[string]bool{})
}

// resolve flattens the Agent named name. A name already on path is a cycle.
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

// mergeResolved overlays b onto acc: a non-empty scalar in b wins and the set
// fields union.
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

// mergeSpec overlays an Agent's own fields onto its merged bases, so the Agent
// overrides everything it extends.
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

// override returns next when set, else cur.
func override(cur, next string) string {
	if next != "" {
		return next
	}
	return cur
}

// union returns the sorted, de-duplicated union of a and b, so a composed bundle is
// the same in any merge order.
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
