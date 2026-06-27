// Package driver makes the agent run-loop a swappable port. The loop, how a run
// reasons and acts on its way to a goal, used to be hardwired: every run was
// assembled from one implicit conversation loop and there was no way to plug a
// different strategy without forking it. A Driver is that strategy as a named,
// resolvable choice: given the ingredients of a run (a model, a toolset, the
// governance grant, the safety brake, the capability-scaffolding plan), it builds
// the executor and the convergence test the goal reconciler drives.
//
// The loop a Driver builds composes WITH the run's governance rather than
// replacing it: a Driver owns the loop shape only. The dispatch waist, the
// capability grant, and the safety brakes sit outside the loop and apply to every
// action it takes, whichever Driver built it. So selecting a different loop never
// widens authority or escapes a halt.
//
// The default driver is the general-purpose software/automation loop, so the
// zero-config experience is unchanged; alternate loops are additive registrations,
// not edits to the default. A run resolves its driver by name from a Registry,
// fail-closed on an unknown name like every other resource, and the choice is fixed
// for the run.
package driver

import (
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
)

// Spec is the loop-agnostic description of one agent run. Every Driver receives the
// same Spec and translates it into its own loop shape, so the ingredients (model,
// tools, governance, observability, scaffolding) stay uniform across loops and only
// the strategy differs. A zero field means that ingredient is absent: no tools, no
// grant (unconstrained), no sandbox gate, no observer, no brake, no scaffolding.
type Spec struct {
	// Model is the language model the loop drives.
	Model llm.Model
	// Tools is the capability surface the loop may call. A loop that takes no tools
	// (a single-shot responder) ignores it.
	Tools []mission.Tool
	// System is the standing system instruction framing every turn.
	System string
	// Grant is the capability grant every action is admitted against at the waist.
	// HasGrant distinguishes "no grant bound" (unconstrained) from the zero grant.
	Grant    capability.Grant
	HasGrant bool
	// Sandbox, when set, gates each action on containment sufficiency at the waist.
	Sandbox sandbox.Sandbox
	// Reporter receives the loop's conversational events for live streaming.
	Reporter mission.Reporter
	// Brakes, when set, halts the run from outside the loop on a tripped breaker or
	// the kill-switch. It applies regardless of which loop is built.
	Brakes *brakes.Hook
	// Plan is the capability-scaffolding for a weaker or more quantized model: how
	// hard the loop should work to keep it reliable. The zero Plan adds nothing, so a
	// capable model runs leanly.
	Plan harness.Plan
}

// Driver builds the run loop for an agent. Name identifies it in the registry and
// is recorded on the run; Build assembles the executor and convergence test from a
// Spec. Build is pure assembly: it wires the loop but does not start it. An
// implementation must be safe for concurrent use.
type Driver interface {
	// Name is the driver's stable identifier, resolved from the registry and
	// recorded on the run's spine.
	Name() string
	// Build assembles the loop for one run. It returns the goal.StepExecutor the
	// worker advances and the goal.StopEvaluator the reconciler converges on.
	Build(s Spec) (goal.StepExecutor, goal.StopEvaluator, error)
}

// Registry resolves a Driver by name. It is fail-closed: an unknown name is an
// error, never a silent fallback to a default, so a run with a misnamed driver
// stops at assembly rather than running the wrong loop. It is safe for concurrent
// use.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{drivers: map[string]Driver{}} }

// Register adds a driver under its Name. It refuses an empty name or a duplicate,
// so a registry can never hold two loops under one name (which would make selection
// ambiguous).
func (r *Registry) Register(d Driver) error {
	if d == nil {
		return fault.New(fault.Terminal, "driver_nil", "driver: refusing to register a nil driver")
	}
	name := d.Name()
	if name == "" {
		return fault.New(fault.Terminal, "driver_empty_name", "driver: refusing to register a driver with an empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.drivers[name]; ok {
		return fault.New(fault.Terminal, "driver_duplicate", "driver: already registered: "+name)
	}
	r.drivers[name] = d
	return nil
}

// Resolve returns the driver registered under name, or a Terminal error naming the
// available drivers when none matches. Resolving the empty name returns the default
// driver, so a run that names no driver gets the blessed general-purpose loop.
func (r *Registry) Resolve(name string) (Driver, error) {
	if name == "" {
		name = NameDefault
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[name]
	if !ok {
		return nil, fault.New(fault.Terminal, "driver_unknown",
			"driver: unknown driver "+name+"; available: "+joinSorted(r.drivers))
	}
	return d, nil
}

// Names lists the registered driver names in sorted order, for diagnostics and a
// kube-style listing.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedNames(r.drivers)
}

// Default returns a registry preloaded with the built-in drivers: the
// general-purpose software loop (the default) and the single-shot responder. A host
// can register additional drivers onto it.
func Default() *Registry {
	r := NewRegistry()
	// The built-ins are constructed in this package, so registration cannot fail; a
	// panic here would mean a programming error in this package, not a runtime fault.
	for _, d := range []Driver{defaultDriver{}, singleShotDriver{}} {
		if err := r.Register(d); err != nil {
			panic("driver: built-in registration failed: " + err.Error())
		}
	}
	return r
}

func sortedNames(m map[string]Driver) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func joinSorted(m map[string]Driver) string {
	names := sortedNames(m)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}
