package state_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/state"
)

// The host-boundary contract: the interfaces a host implements (the state stores)
// or consumes (the model port) to embed the agent. These are the stable surface a
// host depends on, so a change here is a change to that contract and should be a
// deliberate, visible decision rather than an accident.

// boundaryTypes are the stable host-boundary interfaces whose method sets are
// snapshotted below. Engine packages are intentionally excluded: they are
// importable but unstable pre-1.0 (see ARCHITECTURE.md, stability tiers).
func boundaryTypes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"state.Provider":     reflect.TypeOf((*state.Provider)(nil)).Elem(),
		"state.SessionStore": reflect.TypeOf((*state.SessionStore)(nil)).Elem(),
		"state.SkillStore":   reflect.TypeOf((*state.SkillStore)(nil)).Elem(),
		"state.MemoryStore":  reflect.TypeOf((*state.MemoryStore)(nil)).Elem(),
		"llm.Model":          reflect.TypeOf((*llm.Model)(nil)).Elem(),
	}
}

// TestHostBoundaryAPISnapshot pins the method sets of the stable host-boundary
// interfaces to a golden file. Adding, removing, or changing a method changes the
// snapshot and fails here, so growing the contract a host must implement is a
// reviewed change (run the tests with -update to accept it), not a silent one.
func TestHostBoundaryAPISnapshot(t *testing.T) {
	snapshot := make(map[string][]string, len(boundaryTypes()))
	for name, typ := range boundaryTypes() {
		snapshot[name] = methodSet(typ)
	}
	testkit.Golden(t, "host_boundary_api", snapshot)
}

// TestProviderStaysFactoryOfStores guards the host boundary against god-interface
// drift. state.Provider must stay a factory of capability-scoped stores: every
// method besides identity and lifecycle returns a store interface, never domain
// data. Hanging domain methods directly on Provider would force every host to
// implement the whole surface, defeating the "embed a minimal host" promise.
func TestProviderStaysFactoryOfStores(t *testing.T) {
	pt := reflect.TypeOf((*state.Provider)(nil)).Elem()
	for i := 0; i < pt.NumMethod(); i++ {
		m := pt.Method(i)
		switch m.Name {
		case "Name", "Close": // identity and lifecycle, not store accessors
			continue
		}
		ft := m.Type
		if ft.NumIn() != 0 || ft.NumOut() != 1 || ft.Out(0).Kind() != reflect.Interface {
			t.Errorf("Provider.%s is not a store accessor: a host-boundary method must take no args and return a single capability-scoped store interface, "+
				"not domain data. Add the new capability as its own store interface returned from a factory method, not as a method on Provider.", m.Name)
		}
	}
}

// methodSet returns an interface's exported methods as sorted "Name(in...) (out...)"
// signatures, a stable textual form of its public surface.
func methodSet(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		out = append(out, m.Name+signature(m.Type))
	}
	sort.Strings(out)
	return out
}

// signature renders a method's func type (interface methods carry no receiver).
func signature(ft reflect.Type) string {
	in := make([]string, 0, ft.NumIn())
	for i := 0; i < ft.NumIn(); i++ {
		in = append(in, ft.In(i).String())
	}
	out := make([]string, 0, ft.NumOut())
	for i := 0; i < ft.NumOut(); i++ {
		out = append(out, ft.Out(i).String())
	}
	return "(" + strings.Join(in, ", ") + ") (" + strings.Join(out, ", ") + ")"
}
