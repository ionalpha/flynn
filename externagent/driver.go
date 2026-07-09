package externagent

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mcp"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/state"
)

// Driver adapts an external agent CLI to the driver.Driver port, so an external
// harness is a selectable run loop like any other: an additive registry entry, no
// edit to the default loop. Its loop shape is one CLI episode per goal step. It
// composes with governance rather than replacing it: the same grant, containment,
// brake, and event spine that bound a native loop bound every tool call the external
// harness makes, because those sit at the dispatch waist outside the loop.
//
// Construct it with the adapter for the CLI and the spawner that runs the CLI under
// the sandbox's confinement. The workspace is where the CLI reads and where its
// bridged effects land.
type Driver struct {
	adapter Adapter
	spawner Spawner
	workdir string

	// mu guards tiers, which every episode of the run folds its projected events into.
	// A fan-out runs episodes concurrently against the one Driver, so the tally is
	// locked rather than owned by a single step.
	mu    sync.Mutex
	tiers map[Tier]int
}

// NewDriver builds the driver for one external CLI. spawner runs the CLI confined;
// workdir is the directory the episode operates in (the CLI reads it and its bridged
// tools act in it).
func NewDriver(adapter Adapter, spawner Spawner, workdir string) *Driver {
	return &Driver{adapter: adapter, spawner: spawner, workdir: workdir}
}

// Name is the adapter's identifier, the name this loop is selected by and recorded
// under on the run.
func (d *Driver) Name() string { return d.adapter.Name() }

// Tiers returns a copy of the provenance-tier tally of every event the run's episodes
// projected: how many actions the record vouches for at each tier. The host reads it
// after the run to declare the tier mix on the sealed record. Bridged effects are not
// counted here; they are recorded at the dispatch waist as they happen.
func (d *Driver) Tiers() map[Tier]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[Tier]int, len(d.tiers))
	for t, n := range d.tiers {
		out[t] = n
	}
	return out
}

// absorbTiers folds one episode's tier tally into the run's.
func (d *Driver) absorbTiers(m map[Tier]int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tiers == nil {
		d.tiers = make(map[Tier]int, len(m))
	}
	for t, n := range m {
		d.tiers[t] += n
	}
}

// Build assembles the episode loop from the Spec. It captures the governance
// ingredients (tools, grant, sandbox gate, brake, event sink, reporter) and defers
// the wiring to each step, so a step's bridge is scoped and attributed to the goal
// it runs under. The Spec's Model (an llm.Model) is intentionally unused: an
// external harness drives its own model, selected by the goal's model string, not a
// model port.
func (d *Driver) Build(s driver.Spec) (goal.StepExecutor, goal.StopEvaluator, error) {
	return &episodeExec{drv: d, spec: s}, episodeStop{}, nil
}

var _ driver.Driver = (*Driver)(nil)

// episodeExec runs a goal one CLI episode per step. It implements goal.StepExecutor.
type episodeExec struct {
	drv  *Driver
	spec driver.Spec
}

// Execute runs one episode. A resumed step whose checkpoint is already done returns
// it unchanged, so the step is safe to re-run after a crash. Each call assembles the
// governed dispatcher and the loopback bridge scoped to this goal, binds the goal's
// grant and the run's brake onto the context, and runs the episode through the
// spawner; every tool call the CLI makes is admitted, contained, braked, and
// recorded at that waist.
func (e *episodeExec) Execute(ctx context.Context, r resource.Resource) (json.RawMessage, error) {
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "externagent_spec_decode", err)
	}
	status, err := goal.DecodeStatus(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "externagent_status_decode", err)
	}
	cp, err := decodeEpisodeCheckpoint(status.Checkpoint)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "externagent_checkpoint_decode", err)
	}
	if cp.Done {
		return status.Checkpoint, nil
	}

	scope := state.Scope(r.Scope)

	// The governed dispatcher for this goal's bridged tool calls: admit against the
	// grant, gate on the sandbox's containment, apply the brake, and record the
	// lifecycle onto the spine. The same waist a native loop dispatches through.
	dopts := []dispatch.Option{dispatch.WithAdmitter(capability.Admitter{})}
	if e.spec.EventSink != nil {
		dopts = append(dopts, dispatch.WithEventSink(e.spec.EventSink))
	}
	if e.spec.Sandbox != nil {
		dopts = append(dopts, dispatch.WithHook(capability.NewContainmentGate(e.spec.Sandbox)))
	}
	if e.spec.Brakes != nil {
		dopts = append(dopts, dispatch.WithHook(e.spec.Brakes))
	}
	if e.spec.Budget != nil {
		// Charge every bridged tool call against the run's spend pool and refuse one once
		// the ceiling is reached, the same waist a native loop budgets through. The pool is
		// keyed by the run (the goal's own pool, else its name), so `flynn --model
		// codex:<model> --max-cost N` bounds an external run too. The external harness's own
		// inner model calls are outside this waist (unobserved), so the ceiling bounds the
		// governed effects, not the CLI's private provider spend.
		dopts = append(dopts, dispatch.WithHook(e.spec.Budget))
	}
	d := dispatch.New(dopts...)

	server := mcp.NewServer(d, e.spec.Tools, mcp.WithScope(scope), mcp.WithGoal(r.Name))
	runner := NewRunner(e.drv.adapter, server, e.drv.spawner, e.reporter(ctx))

	// Bind the goal's grant and the run's brake onto the context the bridge dispatches
	// under, the same bindings the mission executor sets before it dispatches. The
	// goal's own grant wins; absent it, the Spec's default grant applies; absent both,
	// the run is unconstrained (a standalone run).
	if grant, ok := e.grant(spec); ok {
		ctx = capability.Into(ctx, grant)
	}
	ctx = brakes.Into(ctx, r.Name)
	if e.spec.Budget != nil {
		// Bind the run's spend pool onto the dispatching context so the budget hook can
		// charge it: the goal's own pool when it carries one (a fan-out shares the root's),
		// else the goal's name, which for a single external run is the run id the ceiling
		// was opened under.
		pool := spec.BudgetPool
		if pool == "" {
			pool = r.Name
		}
		ctx = budget.Into(ctx, pool)
	}

	res, err := runner.Run(ctx, Episode{
		Input:   objective(spec),
		Workdir: e.drv.workdir,
		Model:   spec.Model,
		System:  e.system(spec),
	})
	// Fold the episode's tier tally in before the error check: an episode that ran and
	// failed still projected events the record must account for. A start failure yields
	// an empty tally, which folds to nothing.
	e.drv.absorbTiers(res.Tiers)
	if err != nil {
		return nil, err
	}

	cp.Done = true
	cp.Result = res.Text
	cp.Failed = res.Failed
	cp.Err = res.Err
	return encodeEpisodeCheckpoint(cp)
}

// grant returns the capability grant to bind for this goal: the goal's own action
// set when it carries one, else the Spec's default grant, and reports whether any
// grant is bound at all (an unbound grant leaves the run unconstrained).
func (e *episodeExec) grant(spec goal.Spec) (capability.Grant, bool) {
	if len(spec.Grant) > 0 {
		return capability.NewGrant(spec.Grant...), true
	}
	if e.spec.HasGrant {
		return e.spec.Grant, true
	}
	return capability.Grant{}, false
}

// system returns the standing instruction for the episode: the goal's own when set,
// else the Spec's default. It lands as a lower-authority layer, since the external
// harness's own prompt outranks anything injected.
func (e *episodeExec) system(spec goal.Spec) string {
	if spec.System != "" {
		return spec.System
	}
	return e.spec.System
}

// reporter adapts the episode's typed events to the Spec's mission reporter for the
// live trace: assistant text is forwarded as it is produced. A nil Spec reporter
// drops the events.
func (e *episodeExec) reporter(ctx context.Context) func(Event) {
	if e.spec.Reporter == nil {
		return nil
	}
	return func(ev Event) {
		if ev.Kind == EventText && ev.Text != "" {
			e.spec.Reporter.Report(ctx, mission.Event{Kind: mission.EventAssistantText, Text: ev.Text})
		}
	}
}

// objective renders the goal into the episode's opening input: the objective, and
// the stop condition as the explicit definition of done.
func objective(spec goal.Spec) string {
	s := spec.Objective
	if spec.StopCondition != "" {
		s += "\n\nYou are done when: " + spec.StopCondition
	}
	return s
}

// episodeStop converges as soon as an episode has completed the step. It implements
// goal.StopEvaluator.
type episodeStop struct{}

// Met reports whether the episode has finished, returning its final message (or the
// failure) as the reason.
func (episodeStop) Met(_ context.Context, _ goal.Spec, status goal.Status) (bool, string, error) {
	cp, err := decodeEpisodeCheckpoint(status.Checkpoint)
	if err != nil {
		return false, "", fault.Wrap(fault.Terminal, "externagent_checkpoint_decode", err)
	}
	if !cp.Done {
		return false, "", nil
	}
	if cp.Failed {
		reason := cp.Err
		if reason == "" {
			reason = "external agent episode failed"
		}
		return true, reason, nil
	}
	reason := cp.Result
	if reason == "" {
		reason = "external agent episode completed"
	}
	return true, reason, nil
}

// episodeCheckpoint is the loop's resumable state: whether the episode has run, its
// final message, and whether it failed.
type episodeCheckpoint struct {
	Done   bool   `json:"done"`
	Result string `json:"result,omitempty"`
	Failed bool   `json:"failed,omitempty"`
	Err    string `json:"err,omitempty"`
}

func decodeEpisodeCheckpoint(raw json.RawMessage) (episodeCheckpoint, error) {
	var cp episodeCheckpoint
	if len(raw) == 0 {
		return cp, nil
	}
	return cp, json.Unmarshal(raw, &cp)
}

func encodeEpisodeCheckpoint(cp episodeCheckpoint) (json.RawMessage, error) {
	return json.Marshal(cp)
}
