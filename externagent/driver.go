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

// Driver adapts an external CLI to the driver.Driver port as one CLI episode per goal
// step, registered beside the default loop without editing it. The grant,
// containment, brake and event spine that bind a native loop bind every bridged tool
// call too, because they sit at the dispatch waist outside the loop.
type Driver struct {
	adapter Adapter
	spawner Spawner
	workdir string

	// mu guards tiers, steering and the recorder's failure tally, because a fan-out
	// runs episodes concurrently against one Driver.
	mu       sync.Mutex
	tiers    map[Tier]int
	steering Steering
	drift    map[string]int
	lost     int
	lostErr  error

	// recorder receives the harness's attested events. The host sets it before the
	// first episode; nil records nothing.
	recorder Recorder
}

// NewDriver builds the driver for one external CLI. spawner runs the CLI confined;
// workdir is where the CLI reads and its bridged tools act.
func NewDriver(adapter Adapter, spawner Spawner, workdir string) *Driver {
	return &Driver{adapter: adapter, spawner: spawner, workdir: workdir}
}

// Name is the adapter's identifier, which selects this loop and is recorded on the run.
func (d *Driver) Name() string { return d.adapter.Name() }

// Close releases the CLI home the run's episodes shared, with the conversation it
// held. Call it when the run ends.
func (d *Driver) Close() error {
	if c, ok := d.spawner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Tiers returns a copy of the provenance-tier tally of the events the run's episodes
// projected, which the host declares on the sealed record. Bridged effects are not
// counted here; the dispatch waist records them.
func (d *Driver) Tiers() map[Tier]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[Tier]int, len(d.tiers))
	for t, n := range d.tiers {
		out[t] = n
	}
	return out
}

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

// absorbConformance folds one episode's conformance report into the run's. Failed
// probes are counted by name, so a run reports how often the contract drifted.
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

// Steering returns the run's tool-choice counts: bridged tools used versus the
// harness's own. The tool descriptions and preamble are tuned against it.
func (d *Driver) Steering() Steering {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.steering
}

// Drift returns how many episodes failed each conformance probe, by name. Empty means
// every episode honored the contract.
func (d *Driver) Drift() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.drift))
	for name, n := range d.drift {
		out[name] = n
	}
	return out
}

// SetRecorder binds the sink for attested events. The host calls it once, before the
// first episode; it is a setter because detection builds the driver before the run's
// stream exists.
func (d *Driver) SetRecorder(r Recorder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recorder = r
}

// recordAttested persists one attested event, counting a failure instead of failing the
// episode, since the effects are recorded at the waist regardless. The host reports
// the tally, and `flynn spine verify` flags the mismatch between the declared attested
// count and the events recorded.
func (d *Driver) recordAttested(ctx context.Context, ev Event) {
	d.mu.Lock()
	rec := d.recorder
	d.mu.Unlock()
	if rec == nil {
		return
	}
	// Detached from cancellation: after a halt the harness's remaining lines are still
	// drained, and the events just before a halt are the ones worth reading.
	if err := rec.Record(context.WithoutCancel(ctx), ev); err != nil {
		d.mu.Lock()
		d.lost++
		d.lostErr = err
		d.mu.Unlock()
	}
}

// Unrecorded reports how many attested events failed to record, and the last error.
func (d *Driver) Unrecorded() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lost, d.lostErr
}

// Build assembles the episode loop from the Spec and defers wiring to each step, so a
// step's bridge is scoped and attributed to its goal. The Spec's Model is unused: the
// harness drives its own model, chosen by the goal's model string.
func (d *Driver) Build(s driver.Spec) (goal.StepExecutor, goal.StopEvaluator, error) {
	return &episodeExec{drv: d, spec: s}, episodeStop{}, nil
}

var _ driver.Driver = (*Driver)(nil)

// episodeExec runs a goal as one CLI episode per step. It implements goal.StepExecutor.
type episodeExec struct {
	drv  *Driver
	spec driver.Spec
}

// Execute runs one episode with a governed dispatcher and loopback bridge scoped to
// this goal. A step whose checkpoint is already done is returned unchanged, so a
// re-run after a crash is safe.
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

	// counter counts bridged calls at the waist, so steering metrics rest on what the
	// run observed instead of what the harness reported.
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
		// Bridged calls are charged against the run's spend pool and refused at the
		// ceiling, so `flynn --model codex:<model> --max-cost N` bounds an external run.
		// The harness's inner model calls are outside the waist, so the ceiling bounds
		// governed effects only, never the CLI's own provider spend.
		dopts = append(dopts, dispatch.WithHook(exemptProbe{e.spec.Budget}))
	}
	d := dispatch.New(dopts...)

	// The harness's prompt outranks ours, so routing effects through the bridge is only
	// a request; the probes turn its outcome into evidence. The nonce is per episode so
	// compliance cannot be replayed.
	probeTool := NewProbeTool(ids.New())
	// A later turn continues a conversation whose first episode already proved the
	// bridge is reachable; see SessionProbes.
	probes := SessionProbes(probeTool, cp.Session != "")
	toolset := append(append([]mission.Tool{}, e.spec.Tools...), probeTool)

	server := mcp.NewServer(d, toolset, mcp.WithScope(scope), mcp.WithGoal(r.Name))
	runner := NewRunner(e.drv.adapter, server, e.drv.spawner, e.reporter(ctx)).WithProbes(probes)

	// The goal's grant wins, else the Spec's default; with neither the run is
	// unconstrained (a standalone run). The mission executor binds the same.
	if grant, ok := e.grant(spec); ok {
		ctx = capability.Into(ctx, grant)
	}
	ctx = brakes.Into(ctx, r.Name)
	if e.spec.Budget != nil {
		// The goal's pool when it has one (a fan-out shares the root's), else the goal's
		// name, which for a single external run is the run id the ceiling was opened under.
		pool := spec.BudgetPool
		if pool == "" {
			pool = r.Name
		}
		ctx = budget.Into(ctx, pool)
	}

	// A later turn carries its own input; the CLI already holds the conversation, so
	// the objective is not restated.
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
	// Before the error check, because a failed episode still projected events the
	// record must account for.
	e.drv.absorbTiers(res.Tiers)
	if err != nil {
		return nil, err
	}
	// From the waist, not the harness's event stream.
	res.Conformance.Steering.BridgeCalls = counter.count()
	e.drv.absorbConformance(res.Conformance)

	// A failed required probe means the harness cannot or will not reach the bridge, so
	// its record would be entirely attested while looking governed. Terminal, because a
	// harness that ignores the contract once will ignore it on a retry.
	if res.Conformance.Refused() {
		return nil, fault.New(fault.Terminal, "externagent_conformance",
			"the external agent did not honor the session contract: "+res.Conformance.Summary())
	}

	cp.Done = true
	cp.Result = res.Text
	cp.Failed = res.Failed
	cp.Err = res.Err
	// An episode that names no conversation keeps the previous id: not naming it is
	// no evidence it is gone.
	if res.Session != "" {
		cp.Session = res.Session
	}
	return encodeEpisodeCheckpoint(cp)
}

// bridgeCounter counts the tool calls the harness dispatched through the waist, the
// authoritative bridged count. The conformance probe is excluded, since counting it
// would credit the harness with a call it was ordered to make.
type bridgeCounter struct {
	mu sync.Mutex
	n  int
}

// Before counts a dispatched call. Hooks run ahead of the admitter, so a call the grant
// denies is still counted; the native side counts attempts too, and comparing bridged
// successes with native attempts would flatter the steering.
func (c *bridgeCounter) Before(_ context.Context, a dispatch.Action) error {
	if a.Name == ProbeToolName {
		return nil
	}
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

// After has nothing to account for.
func (*bridgeCounter) After(context.Context, dispatch.Action, dispatch.Metering, error) {}

func (c *bridgeCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// exemptProbe keeps a hook's accounting off the conformance probe. The probe uses no
// tokens and has no effect; charging it to the budget or brake would let an exhausted
// budget deny it, which the run would then blame on the harness. The probe stays under
// the admitter and containment gate, which decide whether any action may run.
type exemptProbe struct{ dispatch.Hook }

// Before applies the wrapped hook to every action but the probe.
func (h exemptProbe) Before(ctx context.Context, a dispatch.Action) error {
	if a.Name == ProbeToolName {
		return nil
	}
	return h.Hook.Before(ctx, a)
}

// After applies the wrapped hook's accounting to every action but the probe.
func (h exemptProbe) After(ctx context.Context, a dispatch.Action, m dispatch.Metering, err error) {
	if a.Name == ProbeToolName {
		return
	}
	h.Hook.After(ctx, a, m, err)
}

// grant returns the goal's action set, else the Spec's default, and whether any grant
// is bound (none leaves the run unconstrained).
func (e *episodeExec) grant(spec goal.Spec) (capability.Grant, bool) {
	if len(spec.Grant) > 0 {
		return capability.NewGrant(withProbeAction(spec.Grant)...), true
	}
	if e.spec.HasGrant {
		// A restricted grant is widened by exactly the probe tool, which has no authority
		// of its own, so a grant that omits it cannot fail the probe on a denial the
		// harness never caused.
		if e.spec.Grant.Unrestricted() {
			return e.spec.Grant, true
		}
		return capability.NewGrant(withProbeAction(e.spec.Grant.Actions())...), true
	}
	return capability.Grant{}, false
}

// withProbeAction copies instead of appending in place, because actions is the decoded
// goal spec, which the run must not mutate.
func withProbeAction(actions []string) []string {
	out := make([]string, 0, len(actions)+1)
	out = append(out, actions...)
	return append(out, ProbeToolName)
}

// system returns the goal's standing instruction, else the Spec's default.
func (e *episodeExec) system(spec goal.Spec) string {
	if spec.System != "" {
		return spec.System
	}
	return e.spec.System
}

// reporter sends each event to the mission reporter (assistant text, for the live
// trace) and the recorder (every attested event with its line). Enforced events are
// skipped because the waist records them and writing them twice would double-count.
// It returns nil when neither is bound, so the runner can skip projection.
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

// objective renders the goal's objective, with the stop condition as the definition of
// done.
func objective(spec goal.Spec) string {
	s := spec.Objective
	if spec.StopCondition != "" {
		s += "\n\nYou are done when: " + spec.StopCondition
	}
	return s
}

// episodeStop converges once an episode has completed the step. It implements
// goal.StopEvaluator.
type episodeStop struct{}

// Met reports whether the episode finished, with its final message or failure as the
// reason.
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

// episodeCheckpoint is the external loop's resumable state. The conversation lives in
// the CLI, so this carries its handle where a native loop carries a transcript.
type episodeCheckpoint struct {
	Done   bool   `json:"done"`
	Result string `json:"result,omitempty"`
	Failed bool   `json:"failed,omitempty"`
	Err    string `json:"err,omitempty"`
	// Session is the CLI's id for this goal's conversation, handed back so the next
	// episode continues it.
	Session string `json:"session,omitempty"`
	// Input is the next episode's turn. Empty on the first episode, which runs the
	// objective; ContinueEpisode sets it, so the objective keeps recording what the run
	// set out to do.
	Input string `json:"input,omitempty"`
}

// ContinueEpisode reopens a settled external goal for another turn. It clears the
// completion, sets the turn and keeps the conversation id, so the CLI continues the
// session it holds; a native loop appends a user message instead. The objective is
// left unchanged.
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
	// The prior turn ended, so a fresh turn dispatches a new step instead of waiting on
	// a job whose runtime is gone.
	status.InFlight = nil
	return status, nil
}

func decodeEpisodeCheckpoint(raw json.RawMessage) (episodeCheckpoint, error) {
	var cp episodeCheckpoint
	if len(raw) == 0 {
		return cp, nil
	}
	err := json.Unmarshal(raw, &cp)
	return cp, err
}

func encodeEpisodeCheckpoint(cp episodeCheckpoint) (json.RawMessage, error) {
	return json.Marshal(cp)
}
