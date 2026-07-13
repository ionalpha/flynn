package externagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// TestDriverNameIsTheAdapters proves the loop is selected and recorded under the CLI's own
// identifier, so a record names the harness that drove the run.
func TestDriverNameIsTheAdapters(t *testing.T) {
	if got := NewDriver(NewCodex("", nil), nil, t.TempDir()).Name(); got != "codex" {
		t.Errorf("Name() = %q, want the adapter's name", got)
	}
	if got := NewDriver(NewClaude("", nil), nil, t.TempDir()).Name(); got != "claude" {
		t.Errorf("Name() = %q, want the adapter's name", got)
	}
}

// closerSpawner is a spawner that also holds a resource for the life of the run, the way
// the sandbox spawner holds the harness's credential-and-state home.
type closerSpawner struct {
	fakeSpawner
	mu     sync.Mutex
	closed int
	err    error
}

func (c *closerSpawner) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return c.err
}

func (c *closerSpawner) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// TestDriverCloseReleasesWhatTheRunHeld proves closing the run releases what its episodes
// shared, and that a spawner holding nothing (a test fake) closes to nothing rather than
// failing. The run's credential-and-state home is what the real spawner holds here, so a
// Close that did not reach it would leave the harness's conversation, and whatever else it
// wrote, on disk after the run.
func TestDriverCloseReleasesWhatTheRunHeld(t *testing.T) {
	sp := &closerSpawner{}
	d := NewDriver(NewCodex("", nil), sp, t.TempDir())
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sp.closes() != 1 {
		t.Errorf("the run did not release what its spawner held: %d closes", sp.closes())
	}

	// A close failure is reported, not swallowed: the home is still on disk and the host
	// deserves to hear about it.
	boom := &closerSpawner{err: errors.New("could not remove the credential home")}
	if err := NewDriver(NewCodex("", nil), boom, t.TempDir()).Close(); err == nil {
		t.Error("a failure to release the run's home must be reported")
	}

	// A spawner that holds nothing closes to nothing.
	if err := NewDriver(NewCodex("", nil), fakeSpawner{}, t.TempDir()).Close(); err != nil {
		t.Errorf("a spawner holding nothing should close cleanly: %v", err)
	}
}

// TestRecordAttestedWithoutARecorderIsANoOp proves a run assembled before its stream exists
// (detection happens first, so the recorder is bound later) records nothing rather than
// panicking, and counts nothing as lost: there was no record to miss.
func TestRecordAttestedWithoutARecorderIsANoOp(t *testing.T) {
	d := NewDriver(NewCodex("", nil), fakeSpawner{}, t.TempDir())
	d.recordAttested(context.Background(), Event{Kind: EventText, Tier: TierAttested})
	if lost, err := d.Unrecorded(); lost != 0 || err != nil {
		t.Errorf("an unbound recorder must lose nothing, got %d / %v", lost, err)
	}
}

// TestBridgeCounterCountsAttemptsButNotTheProbe pins the unit the steering ratio rests on.
// The count is of what the harness ATTEMPTED at the waist, including a call the grant went
// on to deny: the native side of the ratio counts attempts too (a command the CLI's own
// sandbox declined is still a turn spent reaching for the wrong tool), so counting only
// bridged successes would flatter the steering. The conformance probe is excluded because it
// is the run's own instrument: counting it would credit the harness with a bridged call it
// was ordered to make.
func TestBridgeCounterCountsAttemptsButNotTheProbe(t *testing.T) {
	c := &bridgeCounter{}
	ctx := context.Background()
	for _, name := range []string{"read", "write", ProbeToolName, "read"} {
		if err := c.Before(ctx, dispatch.Action{Name: name}); err != nil {
			t.Fatalf("Before(%s): %v", name, err)
		}
		// After has nothing to account for; it must not disturb the count.
		c.After(ctx, dispatch.Action{Name: name}, dispatch.Metering{}, nil)
	}
	if got := c.count(); got != 3 {
		t.Errorf("count = %d, want 3 (every attempt, the probe excluded)", got)
	}
}

// recordingHook is a governance hook that records which actions reached it, standing in for
// the run's brake or its spend ceiling.
type recordingHook struct {
	mu            sync.Mutex
	before, after []string
	beforeErr     error
}

func (h *recordingHook) Before(_ context.Context, a dispatch.Action) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.before = append(h.before, a.Name)
	return h.beforeErr
}

func (h *recordingHook) After(_ context.Context, a dispatch.Action, _ dispatch.Metering, _ error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.after = append(h.after, a.Name)
}

func (h *recordingHook) saw() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.before...), append([]string{}, h.after...)
}

// TestExemptProbeKeepsAccountingOffTheRunsOwnInstrument is why the probe is exempt from the
// brake and the spend ceiling. The probe consumes no model tokens and lands no effect; it
// exists only to measure whether the harness honors the contract. If an exhausted budget or
// a tripped brake could deny it, the run would read its own refusal as the harness declining
// to cooperate, and report the failure against the harness when the run itself caused it.
// Everything that is not the probe still goes through the hook, both on the way in and on
// the way out.
func TestExemptProbeKeepsAccountingOffTheRunsOwnInstrument(t *testing.T) {
	inner := &recordingHook{beforeErr: errors.New("over budget")}
	h := exemptProbe{inner}
	ctx := context.Background()

	if err := h.Before(ctx, dispatch.Action{Name: ProbeToolName}); err != nil {
		t.Fatalf("the run's own instrument must not be refused by its accounting: %v", err)
	}
	h.After(ctx, dispatch.Action{Name: ProbeToolName}, dispatch.Metering{}, nil)

	if err := h.Before(ctx, dispatch.Action{Name: "write"}); err == nil {
		t.Fatal("a real action must still be refused when the wrapped hook refuses it")
	}
	h.After(ctx, dispatch.Action{Name: "write"}, dispatch.Metering{}, nil)

	before, after := inner.saw()
	if len(before) != 1 || before[0] != "write" {
		t.Errorf("the wrapped hook saw %v on the way in, want only the real action", before)
	}
	if len(after) != 1 || after[0] != "write" {
		t.Errorf("the wrapped hook saw %v on the way out, want only the real action", after)
	}
}

// TestGrantBindsTheGoalsOwnGrantThenTheSpecs proves which grant an episode is admitted
// against, and that every one of them admits the conformance probe. A run whose grant omits
// the probe would fail its own required probe on a denial the harness never caused, and be
// refused for a contract it in fact honored. Widening by the probe grants no authority: the
// tool touches no file, runs no command, and reaches no network.
func TestGrantBindsTheGoalsOwnGrantThenTheSpecs(t *testing.T) {
	granted := func(g capability.Grant) []string { return g.Actions() }

	// The goal's own grant wins, widened by exactly the probe.
	e := &episodeExec{drv: NewDriver(NewCodex("", nil), fakeSpawner{}, t.TempDir()), spec: driver.Spec{}}
	g, ok := e.grant(goal.Spec{Grant: []string{"read", "write"}})
	if !ok {
		t.Fatal("a goal carrying a grant must be bound")
	}
	if !containsArg(granted(g), ProbeToolName) || !containsArg(granted(g), "read") {
		t.Errorf("the goal's grant must be widened by the probe and keep its own actions: %v", granted(g))
	}

	// Absent a goal grant, the Spec's default applies, also widened by the probe.
	e.spec = driver.Spec{Grant: capability.NewGrant("read"), HasGrant: true}
	g, ok = e.grant(goal.Spec{})
	if !ok {
		t.Fatal("the Spec's default grant must be bound when the goal carries none")
	}
	if !containsArg(granted(g), ProbeToolName) || !containsArg(granted(g), "read") {
		t.Errorf("the default grant must be widened by the probe: %v", granted(g))
	}

	// An unrestricted grant already allows the probe and is passed through untouched.
	e.spec = driver.Spec{Grant: capability.AllowAll(), HasGrant: true}
	g, ok = e.grant(goal.Spec{})
	if !ok || !g.Unrestricted() {
		t.Errorf("an unrestricted grant must be bound as it is, got ok=%v unrestricted=%v", ok, g.Unrestricted())
	}

	// With neither, no grant is bound: the run is unconstrained (a standalone run).
	e.spec = driver.Spec{}
	if _, ok := e.grant(goal.Spec{}); ok {
		t.Error("with no grant anywhere, none must be bound")
	}
}

// TestWithProbeActionDoesNotMutateTheDecodedSpec proves widening a grant by the probe copies
// the actions rather than appending in place. The slice it is handed is the decoded goal
// spec, which the run must not rewrite: a mutation there would leak the probe into the
// goal's recorded grant.
func TestWithProbeActionDoesNotMutateTheDecodedSpec(t *testing.T) {
	actions := []string{"read", "write"}
	out := withProbeAction(actions)
	if len(actions) != 2 || actions[0] != "read" || actions[1] != "write" {
		t.Errorf("the caller's slice was mutated: %v", actions)
	}
	if len(out) != 3 || out[2] != ProbeToolName {
		t.Errorf("the probe was not appended: %v", out)
	}
}

// TestSystemPrefersTheGoalsOwnInstruction proves the standing instruction an episode layers
// in is the goal's when it carries one and the Spec's default otherwise, so a per-goal
// instruction is not silently replaced by the run-wide one.
func TestSystemPrefersTheGoalsOwnInstruction(t *testing.T) {
	e := &episodeExec{spec: driver.Spec{System: "the run's default"}}
	if got := e.system(goal.Spec{System: "this goal's own"}); got != "this goal's own" {
		t.Errorf("system = %q, want the goal's own instruction", got)
	}
	if got := e.system(goal.Spec{}); got != "the run's default" {
		t.Errorf("system = %q, want the Spec's default", got)
	}
}

// TestObjectiveCarriesTheDefinitionOfDone proves the opening turn states the stop condition
// as the explicit definition of done, and that a goal without one is put to the harness as
// the bare objective rather than with a dangling heading.
func TestObjectiveCarriesTheDefinitionOfDone(t *testing.T) {
	got := objective(goal.Spec{Objective: "write the file", StopCondition: "out.txt exists"})
	if !strings.Contains(got, "write the file") || !strings.Contains(got, "You are done when: out.txt exists") {
		t.Errorf("objective = %q, want the objective and its definition of done", got)
	}
	if got := objective(goal.Spec{Objective: "just this"}); got != "just this" {
		t.Errorf("objective with no stop condition = %q, want the bare objective", got)
	}
}

// TestEpisodeStopReportsTheOutcome proves the convergence test reads the episode's own
// result off the checkpoint: an unfinished episode has not converged, a finished one
// converges carrying its final message, and a failed one converges carrying the failure. An
// episode that finished with nothing to say still gets a reason, because an empty reason on
// a converged goal reads as a missing record rather than a quiet success.
func TestEpisodeStopReportsTheOutcome(t *testing.T) {
	cases := []struct {
		name       string
		cp         episodeCheckpoint
		wantMet    bool
		wantReason string
	}{
		{"not yet run", episodeCheckpoint{}, false, ""},
		{"completed with a message", episodeCheckpoint{Done: true, Result: "all done"}, true, "all done"},
		{"completed with nothing to say", episodeCheckpoint{Done: true}, true, "external agent episode completed"},
		{"failed with a reason", episodeCheckpoint{Done: true, Failed: true, Err: "provider refused"}, true, "provider refused"},
		{"failed with no reason", episodeCheckpoint{Done: true, Failed: true}, true, "external agent episode failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := encodeEpisodeCheckpoint(tc.cp)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			met, reason, err := episodeStop{}.Met(context.Background(), goal.Spec{}, goal.Status{Checkpoint: raw})
			if err != nil {
				t.Fatalf("Met: %v", err)
			}
			if met != tc.wantMet || reason != tc.wantReason {
				t.Errorf("Met = (%v, %q), want (%v, %q)", met, reason, tc.wantMet, tc.wantReason)
			}
		})
	}
}

// TestEpisodeStopRefusesACorruptCheckpoint proves an unreadable checkpoint is a terminal
// failure rather than a goal quietly reported as unconverged, which would loop the
// reconciler on a step it can never finish.
func TestEpisodeStopRefusesACorruptCheckpoint(t *testing.T) {
	_, _, err := episodeStop{}.Met(context.Background(), goal.Spec{}, goal.Status{Checkpoint: json.RawMessage(`{"done":`)})
	if err == nil {
		t.Fatal("a corrupt checkpoint must not read as an unconverged goal")
	}
}

// TestContinueEpisodeReopensTheGoalWithoutLosingTheConversation is what a later turn of an
// interactive session rests on. The conversation lives inside the CLI: this side never saw
// it and could not replay it. So reopening the goal must clear the completion (or nothing
// would drive a new episode) and set the new turn, while keeping the conversation id, and it
// must drop any record of the in-flight step, whose runtime is gone. The objective is left
// alone: it records what the run set out to do, and a turn is not a new objective.
func TestContinueEpisodeReopensTheGoalWithoutLosingTheConversation(t *testing.T) {
	settled, err := encodeEpisodeCheckpoint(episodeCheckpoint{
		Done: true, Result: "the last answer", Failed: true, Err: "and it failed", Session: "sess-9",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	status := goal.Status{
		Checkpoint: settled,
		Phase:      goal.PhaseConverged,
		Message:    "converged",
		Steps:      4,
		InFlight:   &goal.InFlight{JobID: "the job that is gone"},
	}

	next, err := ContinueEpisode(status, "and now the second line")
	if err != nil {
		t.Fatalf("ContinueEpisode: %v", err)
	}
	cp, err := decodeEpisodeCheckpoint(next.Checkpoint)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cp.Done || cp.Failed || cp.Result != "" || cp.Err != "" {
		t.Errorf("the prior turn's outcome was not cleared: %+v", cp)
	}
	if cp.Input != "and now the second line" {
		t.Errorf("the new turn did not reach the checkpoint: %q", cp.Input)
	}
	if cp.Session != "sess-9" {
		t.Errorf("the CLI's conversation was dropped: %q; the harness would be told to resume nothing", cp.Session)
	}
	if next.Phase != goal.PhasePending {
		t.Errorf("a reopened goal must be pending, or nothing would drive it: %v", next.Phase)
	}
	if next.Steps != 0 || next.Message != "" {
		t.Errorf("the prior turn's progress was carried forward: steps=%d message=%q", next.Steps, next.Message)
	}
	if next.InFlight != nil {
		t.Errorf("a fresh turn must not wait on a step whose runtime is gone: %s", next.InFlight)
	}
}

// TestContinueEpisodeRefusesACorruptCheckpoint proves a checkpoint that cannot be read is a
// terminal failure and leaves the status untouched, rather than silently reopening the goal
// on a blank conversation the CLI never held.
func TestContinueEpisodeRefusesACorruptCheckpoint(t *testing.T) {
	status := goal.Status{Checkpoint: json.RawMessage(`{"done":`), Phase: goal.PhaseConverged}
	got, err := ContinueEpisode(status, "next")
	if err == nil {
		t.Fatal("a corrupt checkpoint must not be reopened")
	}
	if got.Phase != goal.PhaseConverged {
		t.Errorf("a refused reopen must leave the status as it was, got phase %v", got.Phase)
	}
}

// TestExecuteRefusesUndecodableResources proves a goal whose spec, status, or checkpoint
// cannot be read is a terminal failure rather than an episode run on a half-decoded goal.
// Running one would put an objective the run cannot see to an external harness.
func TestExecuteRefusesUndecodableResources(t *testing.T) {
	d := NewDriver(NewCodex("", nil), fakeSpawner{start: func(context.Context, Episode, Invocation) (Process, error) {
		return nil, errors.New("no episode may be spawned for an undecodable goal")
	}}, t.TempDir())
	exec, _, err := d.Build(driver.Spec{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	cases := []struct {
		name string
		r    resource.Resource
	}{
		{"corrupt spec", resource.Resource{Name: "g1", Spec: json.RawMessage(`{"objective":`)}},
		{"corrupt status", resource.Resource{Name: "g1", Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{"phase":`)}},
		{
			"corrupt checkpoint",
			resource.Resource{Name: "g1", Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{"checkpoint":{"done":"not a bool"}}`)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.Execute(context.Background(), tc.r); err == nil {
				t.Fatal("an undecodable goal must not run an episode")
			}
		})
	}
}

// TestExecuteReportsAFailureToStart proves an episode that could not be launched fails the
// step rather than being recorded as a completed one with nothing in it, which would settle
// the goal on a turn that never ran.
func TestExecuteReportsAFailureToStart(t *testing.T) {
	d := NewDriver(NewCodex("", nil), fakeSpawner{start: func(context.Context, Episode, Invocation) (Process, error) {
		return nil, errors.New("the confined child could not be launched")
	}}, t.TempDir())
	exec, _, err := d.Build(driver.Spec{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", goal.Spec{Objective: "x"}, nil)); err == nil {
		t.Fatal("an episode that never started must fail the step")
	}
	// A start failure projected no events, so the run's tier tally stays empty rather than
	// declaring an account the record does not hold.
	if n := len(d.Tiers()); n != 0 {
		t.Errorf("a failed start folded %d tier(s) into the run's tally", n)
	}
}

// capturingReporter collects the mission events an episode streamed to the live trace.
type capturingReporter struct {
	mu     sync.Mutex
	events []mission.Event
}

func (r *capturingReporter) Report(_ context.Context, ev mission.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *capturingReporter) reported() []mission.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mission.Event{}, r.events...)
}

// TestReporterStreamsOnlyAssistantTextToTheLiveTrace proves the two destinations an
// episode's events fan out to are kept apart. The live trace shows the harness's
// assistant-visible text as it is produced and nothing else: a progress line or a done event
// is not something to show a watching user. The record, meanwhile, keeps every attested
// event. An event the run enforced is not recorded here at all, because the dispatch waist
// records those as they cross it, and writing them twice would double-count the harness's
// claim against the run's own observation.
func TestReporterStreamsOnlyAssistantTextToTheLiveTrace(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = fmt.Fprintln(pw, `{"type":"thread.started","thread_id":"th-1"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"here you go"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	rep := &capturingReporter{}
	spec.Reporter = rep

	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", goal.Spec{Objective: "speak"}, nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := rep.reported()
	if len(got) != 1 {
		t.Fatalf("the live trace saw %d event(s), want only the assistant's text: %+v", len(got), got)
	}
	if got[0].Kind != mission.EventAssistantText || got[0].Text != "here you go" {
		t.Errorf("the live trace event = %+v, want the assistant's text", got[0])
	}
}

// TestReporterIsSkippedWhenNothingWantsTheEvents proves the runner is handed no projection
// callback when neither a live trace nor a record is bound, so a headless, unrecorded run
// does no work per event that nothing will read.
func TestReporterIsSkippedWhenNothingWantsTheEvents(t *testing.T) {
	d := NewDriver(NewCodex("", nil), fakeSpawner{}, t.TempDir())
	e := &episodeExec{drv: d, spec: driver.Spec{}}
	if e.reporter(context.Background()) != nil {
		t.Error("with neither a trace nor a record bound, there is nothing to project events to")
	}

	// Binding either one is enough to want them.
	d.SetRecorder(&captureRecorder{})
	if e.reporter(context.Background()) == nil {
		t.Error("a bound record must receive the harness's account")
	}
	e2 := &episodeExec{drv: NewDriver(NewCodex("", nil), fakeSpawner{}, t.TempDir()), spec: driver.Spec{Reporter: &capturingReporter{}}}
	if e2.reporter(context.Background()) == nil {
		t.Error("a bound live trace must receive the harness's text")
	}
}
