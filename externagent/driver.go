package externagent

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/ids"
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

	// mu guards tiers and steering, which every episode of the run folds its projected
	// events into, and the recorder's failure tally. A fan-out runs episodes concurrently
	// against the one Driver, so these are locked rather than owned by a single step.
	mu       sync.Mutex
	tiers    map[Tier]int
	steering Steering
	drift    map[string]int
	lost     int
	lostErr  error

	// recorder persists the harness's attested events onto the run's record. It is set
	// by the host once the run's stream exists, before the first episode runs, and read
	// by every episode after. Nil records nothing.
	recorder Recorder
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

// Close releases what the run's episodes shared: the harness's credential-and-state home,
// where the CLI kept the conversation this run's turns continued. It is called when the run
// ends. A spawner that holds nothing (a test fake) closes to nothing.
func (d *Driver) Close() error {
	if c, ok := d.spawner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

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

// absorbConformance folds one episode's conformance report into the run's: the steering
// counts add up, and each probe the harness failed is counted by name so a run reports
// how often the contract drifted, not merely whether it drifted last.
func (d *Driver) absorbConformance(rep ConformanceReport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := &d.steering
	s.BridgeCalls += rep.Steering.BridgeCalls
	s.ForeignCalls += rep.Steering.ForeignCalls
	s.NativeCommands += rep.Steering.NativeCommands
	s.NativeDeclined += rep.Steering.NativeDeclined
	if d.drift == nil {
		d.drift = map[string]int{}
	}
	for _, f := range rep.Failed() {
		d.drift[f.Name]++
	}
}

// Steering returns the run's tool-choice counts across every episode: how often the
// harness used the bridged tools it was told to use, and how often it reached for its
// own instead. It is the number the tool descriptions and the preamble are tuned against.
func (d *Driver) Steering() Steering {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.steering
}

// Drift returns how many episodes failed each conformance probe, by probe name. An empty
// map means the harness honored the session contract on every episode.
func (d *Driver) Drift() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.drift))
	for name, n := range d.drift {
		out[name] = n
	}
	return out
}

// SetRecorder binds the sink the harness's attested events are recorded to. The host
// calls it once, after the run's stream exists and before the first episode runs; the
// driver is constructed earlier (detection happens before a run is assembled), which is
// why this is a setter rather than a constructor argument.
func (d *Driver) SetRecorder(r Recorder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recorder = r
}

// recordAttested persists one attested event, counting a failure instead of failing the
// episode: the run's effects are enforced and recorded at the waist whatever happens
// here, so a lost line is a hole in the harness's account, not a reason to abandon the
// work. The hole is not silent - the host reports the tally, and the record's declared
// attested count will not match the events it carries, which `flynn spine verify` calls
// out.
func (d *Driver) recordAttested(ctx context.Context, ev Event) {
	d.mu.Lock()
	rec := d.recorder
	d.mu.Unlock()
	if rec == nil {
		return
	}
	// Detach the append from the episode's cancellation. A halt kills the subprocess and
	// cancels this context, but the lines the harness already wrote are still drained and
	// projected; recording them under the cancelled context would drop exactly the events
	// leading up to the halt, which are the ones worth reading.
	if err := rec.Record(context.WithoutCancel(ctx), ev); err != nil {
		d.mu.Lock()
		d.lost++
		d.lostErr = err
		d.mu.Unlock()
	}
}

// Unrecorded reports how many of the harness's attested events could not be written to
// the record, and the last failure. Zero and nil mean the record carries the harness's
// whole account.
func (d *Driver) Unrecorded() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lost, d.lostErr
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
	// Counts the bridged calls the harness dispatched, so the steering metrics rest on
	// what the waist observed rather than on what the harness said about itself.
	counter := &bridgeCounter{}
	dopts := []dispatch.Option{dispatch.WithAdmitter(capability.Admitter{}), dispatch.WithHook(counter)}
	if e.spec.EventSink != nil {
		dopts = append(dopts, dispatch.WithEventSink(e.spec.EventSink))
	}
	if e.spec.Sandbox != nil {
		dopts = append(dopts, dispatch.WithHook(capability.NewContainmentGate(e.spec.Sandbox)))
	}
	if e.spec.Brakes != nil {
		dopts = append(dopts, dispatch.WithHook(exemptProbe{e.spec.Brakes}))
	}
	if e.spec.Budget != nil {
		// Charge every bridged tool call against the run's spend pool and refuse one once
		// the ceiling is reached, the same waist a native loop budgets through. The pool is
		// keyed by the run (the goal's own pool, else its name), so `flynn --model
		// codex:<model> --max-cost N` bounds an external run too. The external harness's own
		// inner model calls are outside this waist (unobserved), so the ceiling bounds the
		// governed effects, not the CLI's private provider spend.
		dopts = append(dopts, dispatch.WithHook(exemptProbe{e.spec.Budget}))
	}
	d := dispatch.New(dopts...)

	// The episode opens with conformance probes: the harness's own prompt outranks
	// anything injected here, so the contract that it route effects through the bridged
	// tools is a request. The probes turn the request's outcome into evidence. The nonce
	// is per episode, so compliance cannot be replayed from an earlier one.
	probeTool := NewProbeTool(ids.New())
	// A later turn of a session continues a conversation this run already opened, and the
	// episode that opened it proved the harness reaches the bridge. See SessionProbes for
	// why that makes the probe evidence rather than a gate from the second turn on.
	probes := SessionProbes(probeTool, cp.Session != "")
	toolset := append(append([]mission.Tool{}, e.spec.Tools...), probeTool)

	server := mcp.NewServer(d, toolset, mcp.WithScope(scope), mcp.WithGoal(r.Name))
	runner := NewRunner(e.drv.adapter, server, e.drv.spawner, e.reporter(ctx)).WithProbes(probes)

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

	// The turn to put to the harness: a later turn of an interactive session carries its
	// own input, and the CLI already holds the conversation the earlier turns built, so
	// the episode continues that conversation rather than restating the objective.
	input := cp.Input
	if input == "" {
		input = objective(spec)
	}
	res, err := runner.Run(ctx, Episode{
		Input:   input,
		Workdir: e.drv.workdir,
		Model:   spec.Model,
		System:  e.system(spec),
		Probes:  Instructions(probes),
		Session: cp.Session,
	})
	// Fold the episode's tier tally in before the error check: an episode that ran and
	// failed still projected events the record must account for. A start failure yields
	// an empty tally, which folds to nothing.
	e.drv.absorbTiers(res.Tiers)
	if err != nil {
		return nil, err
	}
	// The bridged count comes from the waist, not the episode's event stream.
	res.Conformance.Steering.BridgeCalls = counter.count()
	e.drv.absorbConformance(res.Conformance)

	// A required probe failed: the harness could not or would not reach the bridge, so
	// nothing it does can be enforced and its record would be entirely attested while
	// looking like a governed run. Refuse the episode rather than produce that record.
	// The failure is terminal, since a harness that ignores the contract on one episode
	// will ignore it on a retry.
	if res.Conformance.Refused() {
		return nil, fault.New(fault.Terminal, "externagent_conformance",
			"the external agent did not honor the session contract: "+res.Conformance.Summary())
	}

	cp.Done = true
	cp.Result = res.Text
	cp.Failed = res.Failed
	cp.Err = res.Err
	// Keep the conversation the CLI reported, so a later turn continues it. A CLI that
	// announced none leaves the previous id in place rather than clearing it: an episode
	// that could not name its conversation is not evidence the conversation is gone.
	if res.Session != "" {
		cp.Session = res.Session
	}
	return encodeEpisodeCheckpoint(cp)
}

// bridgeCounter counts the tool calls the external harness actually dispatched through
// the waist. It is the authoritative count of bridged calls: unlike the harness's event
// stream, which is its account of itself, this counts what the run admitted and ran.
//
// The conformance probe is not counted. It is the run's own instrument, and counting it
// would credit the harness with a bridged call it was ordered to make.
type bridgeCounter struct {
	mu sync.Mutex
	n  int
}

// Before counts a dispatched call. Hooks run ahead of the admitter, so this counts every
// call the harness attempted, including one the grant went on to deny. That is the right
// unit: the native side of the ratio counts attempts too (a command the CLI's sandbox
// declined is still a turn spent reaching for the wrong tool), and a ratio of bridged
// successes to native attempts would flatter the steering.
func (c *bridgeCounter) Before(_ context.Context, a dispatch.Action) error {
	if a.Name == ProbeToolName {
		return nil
	}
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

// After is required by the hook port and has nothing to account for.
func (*bridgeCounter) After(context.Context, dispatch.Action, dispatch.Metering, error) {}

// count reports how many bridged calls the harness dispatched.
func (c *bridgeCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// exemptProbe wraps a governance hook so it does not apply to the conformance probe.
//
// The probe is the run's own instrument, not work the harness asked for: it consumes no
// model tokens, lands no effect, and exists only to measure whether the harness honors
// the contract. Charging it against the run's spend ceiling or its runaway brake would
// let an exhausted budget or a tripped brake deny the probe, which the run would then
// read as the harness refusing to cooperate. The failure would be reported against the
// harness when the run itself caused it.
//
// It stays under the admitter and the containment gate. Those decide whether an action
// may run at all, and the probe should be no exception; what it is exempt from is the
// accounting of work it does not do.
type exemptProbe struct{ dispatch.Hook }

// Before skips the wrapped hook for the probe action and applies it to everything else.
func (h exemptProbe) Before(ctx context.Context, a dispatch.Action) error {
	if a.Name == ProbeToolName {
		return nil
	}
	return h.Hook.Before(ctx, a)
}

// After skips the wrapped hook's accounting for the probe action.
func (h exemptProbe) After(ctx context.Context, a dispatch.Action, m dispatch.Metering, err error) {
	if a.Name == ProbeToolName {
		return
	}
	h.Hook.After(ctx, a, m, err)
}

// grant returns the capability grant to bind for this goal: the goal's own action
// set when it carries one, else the Spec's default grant, and reports whether any
// grant is bound at all (an unbound grant leaves the run unconstrained).
func (e *episodeExec) grant(spec goal.Spec) (capability.Grant, bool) {
	if len(spec.Grant) > 0 {
		return capability.NewGrant(withProbeAction(spec.Grant)...), true
	}
	if e.spec.HasGrant {
		// An unrestricted grant already allows the probe; a restricted one is widened by
		// exactly the probe tool, which grants no authority of its own (it touches no file,
		// runs no command, reaches no network). Without this a run whose grant omits the
		// probe would fail its required probe on a denial the harness never caused.
		if e.spec.Grant.Unrestricted() {
			return e.spec.Grant, true
		}
		return capability.NewGrant(withProbeAction(e.spec.Grant.Actions())...), true
	}
	return capability.Grant{}, false
}

// withProbeAction returns the granted actions plus the probe tool, copied rather than
// appended in place: the caller's slice is the decoded goal spec, which the run must not
// mutate.
func withProbeAction(actions []string) []string {
	out := make([]string, 0, len(actions)+1)
	out = append(out, actions...)
	return append(out, ProbeToolName)
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

// reporter fans each of the episode's typed events out to the two places that want
// them: the Spec's mission reporter, for the live trace (assistant text only, as it is
// produced), and the record, which keeps every attested event with the harness's own
// line. An event the run enforced is not recorded here - the dispatch waist records
// those as they cross it, and writing them twice would double-count the harness's claim
// against the run's observation.
//
// It returns nil only when neither destination is bound, so the runner can skip the
// projection entirely.
func (e *episodeExec) reporter(ctx context.Context) func(Event) {
	e.drv.mu.Lock()
	recording := e.drv.recorder != nil
	e.drv.mu.Unlock()
	if e.spec.Reporter == nil && !recording {
		return nil
	}
	return func(ev Event) {
		if e.spec.Reporter != nil && ev.Kind == EventText && ev.Text != "" {
			e.spec.Reporter.Report(ctx, mission.Event{Kind: mission.EventAssistantText, Text: ev.Text})
		}
		if ev.Tier == TierAttested {
			e.drv.recordAttested(ctx, ev)
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
// final message, whether it failed, the CLI conversation the run holds, and the turn
// to put to it. It is the external loop's answer to the native loop's message list:
// the conversation itself lives inside the CLI, so what is carried here is the handle
// to it, not a transcript this side could never faithfully reconstruct.
type episodeCheckpoint struct {
	Done   bool   `json:"done"`
	Result string `json:"result,omitempty"`
	Failed bool   `json:"failed,omitempty"`
	Err    string `json:"err,omitempty"`
	// Session is the conversation the CLI opened for this goal, as the CLI named it. The
	// next episode hands it back so the harness continues where it left off.
	Session string `json:"session,omitempty"`
	// Input is the turn the next episode runs. Empty on the first episode, which runs the
	// goal's objective; a later turn (ContinueEpisode) sets it, so the objective stays the
	// record of what the run set out to do rather than being overwritten by the last thing
	// the user typed.
	Input string `json:"input,omitempty"`
}

// ContinueEpisode reopens a settled external goal for another turn: it clears the
// episode's completion so the next reconcile runs a new one, sets the turn to put to
// the harness, and keeps the conversation id so the CLI continues the session it
// already holds rather than starting cold. It mirrors what a native loop does by
// appending a user message to its transcript; here the transcript is the CLI's, and
// this is the handle to it.
//
// The goal's objective is left alone: it records what the run set out to do, and the
// turn is not a new objective.
func ContinueEpisode(status goal.Status, text string) (goal.Status, error) {
	cp, err := decodeEpisodeCheckpoint(status.Checkpoint)
	if err != nil {
		return status, fault.Wrap(fault.Terminal, "externagent_checkpoint_decode", err)
	}
	cp.Done = false
	cp.Result = ""
	cp.Failed = false
	cp.Err = ""
	cp.Input = text
	raw, err := encodeEpisodeCheckpoint(cp)
	if err != nil {
		return status, fault.Wrap(fault.Terminal, "externagent_checkpoint_encode", err)
	}
	status.Checkpoint = raw
	status.Phase = goal.PhasePending
	status.Message = ""
	status.Steps = 0
	// Drop any record of an in-flight step: the prior turn has ended (it converged, or it
	// was cancelled mid-episode), so a fresh turn dispatches a new step rather than
	// waiting on a job whose runtime is gone.
	status.InFlight = nil
	return status, nil
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
