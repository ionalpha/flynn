// Package driver makes the agent's run loop a named, swappable choice. A [Driver] takes
// the ingredients of a run ([Spec]) and builds the executor and convergence test the goal
// reconciler drives.
//
// A Driver owns the loop shape only. The dispatch waist, the capability grant and the
// brakes apply to every action whichever Driver built the loop, so a different loop never
// widens authority or escapes a halt. A run resolves its driver by name from a
// [Registry], and the choice is fixed for the run.
package driver

import (
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
)

// Spec describes one agent run independently of the loop. A zero field means the
// ingredient is absent; a missing grant means unconstrained.
type Spec struct {
	Model llm.Model
	// Tools is ignored by a loop that takes none.
	Tools  []mission.Tool
	System string
	// HasGrant tells "no grant bound" (unconstrained) from the zero grant.
	Grant    capability.Grant
	HasGrant bool
	// Sandbox gates each action on containment at the waist.
	Sandbox  sandbox.Sandbox
	Reporter mission.Reporter
	// Brakes halts the run from outside the loop on a tripped breaker or the
	// kill-switch.
	Brakes *brakes.Hook
	// Budget charges every action against the run's spend pool. A pool with no budget
	// resource is unlimited.
	Budget *budget.Hook
	// Fanout offers the model a spawn tool for concurrent child runs. Nil runs a goal as
	// one conversation.
	Fanout mission.Fanout
	// Approval requires a fresh human authorization for the actions its policy lists.
	// Nil requires nothing.
	Approval *Approval
	// Allowance marks irreversible actions outside the workspace, which run only where
	// the goal declared them. It covers delegated children too, so delegating cannot
	// route around it. Nil marks nothing.
	Allowance allowance.Policy
	// EventSink records each governed action's lifecycle on the spine, so admission
	// decisions are part of the sealed history. Nil records nothing extra.
	EventSink dispatch.EventSink
	// CompactionBudget is the input-token count past which the loop elides the oldest
	// middle turns. Zero keeps the default.
	CompactionBudget int
	// Plan is extra scaffolding for a weaker or quantized model. The zero Plan adds
	// nothing.
	Plan harness.Plan
}

// Approval is a run's human-authorization wiring. Gate pauses a privileged action;
// Prompter, Signer and Host resolve the pause by asking a person and minting the
// single-use approval. A Gate with no Prompter refuses, which is right for an unattended
// run. A Prompter without a Signer is ignored.
type Approval struct {
	Gate     *approval.Gate
	Prompter mission.ApprovalPrompter
	Signer   approval.Signer
	Host     string
}

// Driver builds the run loop for an agent. An implementation must be safe for
// concurrent use.
type Driver interface {
	// Name is the stable identifier recorded on the run's spine.
	Name() string
	// Build wires the loop for one run without starting it.
	Build(s Spec) (goal.StepExecutor, goal.StopEvaluator, error)
}

// Registry resolves a Driver by name. An unknown name is an error, so a misnamed driver
// stops the run at assembly. It is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{drivers: map[string]Driver{}} }

// Register adds a driver under its Name. It refuses an empty or duplicate name.
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

// Resolve returns the driver under name, or a Terminal error listing the registered
// names. The empty name resolves to [NameDefault].
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

// Names lists the registered driver names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedNames(r.drivers)
}

// Default returns a registry holding [NameDefault] and [NameSingleShot].
func Default() *Registry {
	r := NewRegistry()
	// Registration of the built-ins fails only on a bug in this package.
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
