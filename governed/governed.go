// Package governed composes a governed Flynn run from backends a host supplies.
//
// Everything that makes a run governed lives here and nowhere else: the confined
// sandbox, the toolset and the grant that bounds it, the dispatch hooks (brakes, spend
// budget, approval, containment), the spine sink, and the run-level judges (verifier,
// auditor, steer judge, refusal and progress probes). The flynn binary is one caller of
// [Assemble]; an embedder is another, and gets the same run. A package that wired these
// by hand would drift from the binary the first time a guard was added to one side.
//
// # Defaults are the governed ones
//
// A run assembled with no options carries every guard the binary does. There is no
// option that means "fewer guards" except [Waive], which names the [Guard], needs a
// reason, and writes both onto the run's spine stream as a [WaivedEventType] event
// before the run starts. A nil or zero value passed to an option is an error, never a
// silent opt-out, and [Run.Posture] reports what was wired so a host can
// assert it.
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
	// DefaultCompactionBudget is the input-token budget past which a run elides the
	// oldest middle turns. It is a conservative floor for a model whose window is
	// unknown; a model plan with a measured, narrower context tightens it.
	DefaultCompactionBudget = 100_000

	// DefaultFanoutWidth caps how many child runs a fan-out may have outstanding at
	// once, bounding the blast radius of delegation alongside the depth guard.
	DefaultFanoutWidth = 8

	// FanoutRootMaxSteps is the step budget a fan-out parent runs under. A fan-out adds
	// orchestration steps a single conversation never takes (a dispatch per delegation,
	// one poll per reconcile while it waits), so the reconciler's default would cut a
	// legitimate delegation off mid-fold.
	FanoutRootMaxSteps = 200

	// DefaultMaxActionsPerMinute is the rate the default safety brake halts a run at. It
	// sits well above any real run's pace so it catches only a degenerate tight loop.
	DefaultMaxActionsPerMinute = 600
)

// Backends are the durable ports a run is assembled over. All three are required: a run
// with no log has no record, and one with no resource store or queue has nothing to
// reconcile, so a nil port is an error rather than an in-memory substitute that is not asked for that
// would lose the run on exit. [MemoryBackends] is the explicit in-memory choice.
type Backends struct {
	// Resources holds the run's goal and every child goal, budget and approval it spawns.
	Resources resource.Store
	// Jobs is the work queue the runtime's workers drain.
	Jobs jobs.Queue
	// Log is the spine the run is recorded on, and the stream waivers are written to.
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
// the process exits; it is the choice for tests and one-shot embedding.
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
