// Package governed composes a governed Flynn run from backends a host supplies.
//
// The sandbox, the grant, the dispatch hooks (brakes, budget, approval, containment), the
// spine sink and the run-level judges are composed only here, so the flynn binary and an
// embedder calling [Assemble] get the same run and cannot drift apart.
//
// # Defaults are the governed ones
//
// A run with no options carries every guard. Only [Waive] removes one: it names the
// [Guard], needs a reason, and records both as a [WaivedEventType] event before the run
// starts. A nil or zero option value is an error, and [Run.Posture] reports what was
// wired so a host can assert it.
//
// # Modes
//
// The run is a single conversation by default. [WithFanout] lets the model delegate to
// governed child goals, [WithExternalAgent] hands the loop to an external agent CLI,
// and [WithToolset] runs a caller-supplied toolset with no sandbox. The last three are
// mutually exclusive.
package governed

import (
	"errors"

	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

const (
	// DefaultCompactionBudget is the input-token count past which a run elides the oldest
	// middle turns. It is a floor for an unknown window; a model plan can tighten it.
	DefaultCompactionBudget = 100_000

	// DefaultFanoutWidth caps a fan-out's outstanding child runs.
	DefaultFanoutWidth = 8

	// FanoutRootMaxSteps is a fan-out parent's step budget. Each delegation and each poll
	// while waiting costs a step, so the reconciler's default would cut delegation short.
	FanoutRootMaxSteps = 200

	// DefaultMaxActionsPerMinute is the default brake rate, set well above any normal
	// run's pace so only a tight loop trips it.
	DefaultMaxActionsPerMinute = 600
)

// Backends are the durable ports a run is assembled over. All three are required, and a
// nil one is an error because a silent in-memory substitute would lose the run on exit.
// [MemoryBackends] is the explicit in-memory choice.
type Backends struct {
	// Resources holds the goal and every child goal, budget and approval it spawns.
	Resources resource.Store
	Jobs      jobs.Queue
	// Log records the run, including its waivers.
	Log spine.Log
}

func (b Backends) validate() error {
	switch {
	case b.Resources == nil:
		return errors.New("governed: Backends.Resources is required")
	case b.Jobs == nil:
		return errors.New("governed: Backends.Jobs is required")
	case b.Log == nil:
		return errors.New("governed: Backends.Log is required")
	}
	return nil
}

// MemoryBackends returns in-process backends over [Registry]. A run on them is lost when
// the process exits.
func MemoryBackends() (Backends, error) {
	reg, err := Registry()
	if err != nil {
		return Backends{}, err
	}
	return Backends{
		Resources: resource.NewMemory(reg),
		Jobs:      jobs.NewMemory(),
		Log:       spine.NewMemoryLog(),
	}, nil
}
