package externagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
)

// TestRequiredProbeFailureRefusesEpisode proves an episode whose harness never reaches
// the bridge is refused rather than recorded. Such a run can produce no enforced effects
// at all, so its record would be entirely attested while looking like a governed run.
func TestRequiredProbeFailureRefusesEpisode(t *testing.T) {
	workdir := t.TempDir()
	// A harness that ignores the contract: it does its turn and never calls the probe.
	spawner := scriptSpawner(func(_ Episode, _ Invocation, pw *io.PipeWriter) {
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	exec, _, err := d.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	gspec := goal.Spec{Objective: "do something", Grant: []string{"read"}}
	_, err = exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil))
	if err == nil {
		t.Fatal("an episode that never reached the bridge must be refused")
	}
	if !strings.Contains(err.Error(), "bridge-reachable") {
		t.Errorf("refusal must name the failed probe, got: %v", err)
	}
	if got := d.Drift()["bridge-reachable"]; got != 1 {
		t.Errorf("drift for bridge-reachable = %d, want 1", got)
	}
}

// TestNarratedProbeCallDoesNotPass is the point of settling the probe on the tool itself
// rather than on the CLI's event stream. Here the harness emits a perfectly-formed
// mcp_tool_call event claiming it called the probe with the right nonce, but never
// dispatches. The stream says it complied; the waist never saw it. The probe must fail.
func TestNarratedProbeCallDoesNotPass(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		nonce := probeNonce(ep)
		// The harness's own account of a call it never made.
		_, _ = fmt.Fprintf(pw,
			`{"type":"item.completed","item":{"type":"mcp_tool_call","server":%q,"tool":%q,"arguments":{"nonce":%q},"status":"completed"}}`+"\n",
			ep.Bridge.Name, ProbeToolName, nonce)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	exec, _, _ := d.Build(spec)

	gspec := goal.Spec{Objective: "narrate", Grant: []string{"read"}}
	_, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil))
	if err == nil {
		t.Fatal("a narrated tool call must not pass a probe settled at the dispatch waist")
	}
	if !strings.Contains(err.Error(), "bridge-reachable") {
		t.Errorf("refusal must name the failed probe, got: %v", err)
	}
}

// TestWrongNonceDoesNotPassProbe proves the probe tests instruction-following and not
// merely reachability: a harness that finds the tool and calls it with its own made-up
// argument has not done what it was told.
func TestWrongNonceDoesNotPassProbe(t *testing.T) {
	tool := NewProbeTool("the-real-nonce")
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"nonce":"guessed"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if tool.Called() {
		t.Error("a call with the wrong nonce must not satisfy the probe")
	}
	if !strings.Contains(out, "not followed") {
		t.Errorf("the tool should say the instruction was not followed, got %q", out)
	}

	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"nonce":"the-real-nonce"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !tool.Called() {
		t.Error("a call with the right nonce must satisfy the probe")
	}
}

// TestNativeToolDriftIsAdvisory proves a harness that reaches for its own shell is
// reported as drift and counted in the steering metrics, but is not refused: its native
// effects cannot land (it runs read-only with approvals denied), so the run is still
// governed. What is lost is observability, which the record already declares.
func TestNativeToolDriftIsAdvisory(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		// One bridged call, two native commands, one of which codex's own sandbox declined.
		_, _ = bridgeClient(ep.Bridge, "read", `{"path":"nothing.txt"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"command_execution","command":"ls -la","status":"completed"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"command_execution","command":"rm -rf /","status":"declined"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	exec, _, _ := d.Build(spec)

	gspec := goal.Spec{Objective: "read a file", Grant: []string{"read"}}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil)); err != nil {
		t.Fatalf("an advisory probe failure must not refuse the episode: %v", err)
	}

	if got := d.Drift()["no-native-tools"]; got != 1 {
		t.Errorf("drift for no-native-tools = %d, want 1", got)
	}
	if got := d.Drift()["bridge-reachable"]; got != 0 {
		t.Errorf("bridge-reachable should have passed, drift = %d", got)
	}

	s := d.Steering()
	if s.NativeCommands != 2 || s.NativeDeclined != 1 {
		t.Errorf("native counts = %d commands / %d declined, want 2 / 1", s.NativeCommands, s.NativeDeclined)
	}
	// The bridged read is counted at the waist even though this scripted harness never
	// reported it on its event stream. The probe call is not counted: it is the run's own
	// instrument, not a tool the harness chose. So one bridged call against two native
	// ones.
	if s.BridgeCalls != 1 {
		t.Errorf("bridged calls = %d, want 1 (counted at the waist, probe excluded)", s.BridgeCalls)
	}
	if got := s.NativeRate(); got < 0.66 || got > 0.67 {
		t.Errorf("native rate = %v, want 2/3", got)
	}
}

// TestBridgedCallsCountedAtWaistNotFromHarnessAccount pins the reason the bridged count
// does not come from the event stream. This harness dispatches a real bridged call and
// never reports it, exactly as a harness with a quiet event stream would. Counting from
// its account would show zero bridged calls and a 100% native rate, condemning steering
// that in fact worked.
func TestBridgedCallsCountedAtWaistNotFromHarnessAccount(t *testing.T) {
	workdir := t.TempDir()
	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		satisfyProbe(ep)
		_, _ = bridgeClient(ep.Bridge, "read", `{"path":"nothing.txt"}`)
		_, _ = bridgeClient(ep.Bridge, "read", `{"path":"other.txt"}`)
		// Not one event reporting those calls.
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	d, spec := driverWith(t, workdir, spawner)
	exec, _, _ := d.Build(spec)

	gspec := goal.Spec{Objective: "read", Grant: []string{"read"}}
	if _, err := exec.Execute(context.Background(), goalResource(t, "g1", gspec, nil)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	s := d.Steering()
	if s.BridgeCalls != 2 {
		t.Errorf("bridged calls = %d, want 2 (observed at the waist, unreported by the harness)", s.BridgeCalls)
	}
	if s.NativeRate() != 0 {
		t.Errorf("native rate = %v, want 0: the harness used only bridged tools", s.NativeRate())
	}
}

// TestSteeringCountsForeignCallsApartFromBridged proves a call the harness makes on some
// other MCP server is counted separately: the run neither serves nor governs it, so it is
// neither a bridged call nor native tool use. A call on the run's own bridge contributes
// nothing here, because the waist counts those directly.
func TestSteeringCountsForeignCallsApartFromBridged(t *testing.T) {
	var s Steering
	const bridge = "flynn"
	s.absorbSteering(Event{Kind: EventBridgeCall, Server: bridge, Tool: "read"}, bridge)
	s.absorbSteering(Event{Kind: EventBridgeCall, Server: "someone_elses_server", Tool: "read"}, bridge)
	// A call reporting no server is treated as the run's own, since it is the only server
	// the episode is configured with. It is left to the waist to count.
	s.absorbSteering(Event{Kind: EventBridgeCall, Tool: "write"}, bridge)
	s.absorbSteering(Event{Kind: EventNativeCommand, Status: "completed"}, bridge)

	if s.BridgeCalls != 0 {
		t.Errorf("bridged calls = %d, want 0: the stream does not count them, the waist does", s.BridgeCalls)
	}
	if s.ForeignCalls != 1 || s.NativeCommands != 1 {
		t.Fatalf("counts = %d foreign / %d native, want 1 / 1", s.ForeignCalls, s.NativeCommands)
	}

	// With the waist's count folded in, as the driver does, the rate is over every attempt.
	s.BridgeCalls = 2
	if s.Total() != 4 {
		t.Errorf("total = %d, want 4", s.Total())
	}
	if got := s.NativeRate(); got != 0.25 {
		t.Errorf("native rate = %v, want 0.25", got)
	}
}

// TestSteeringEmptyEpisodeHasNoRate proves an episode that attempted no tools reports a
// zero rate rather than dividing by zero: there was nothing to steer.
func TestSteeringEmptyEpisodeHasNoRate(t *testing.T) {
	var s Steering
	s.absorbSteering(Event{Kind: EventText, Text: "just talking"}, "flynn")
	if s.Total() != 0 || s.NativeRate() != 0 {
		t.Errorf("an episode with no tool attempts: total %d, rate %v, want 0 / 0", s.Total(), s.NativeRate())
	}
}

// TestPromptLayersRenderOrder pins the authority demotion the translator expresses: the
// run's standing instruction, then the probes, then the objective, separated so the
// harness reads them as distinct blocks. The probes sit before the objective because
// instruction-following decays as the turn gets longer.
func TestPromptLayersRenderOrder(t *testing.T) {
	got := promptLayers{
		system: "STANDING",
		probes: []string{"PROBE-A", "PROBE-B"},
		input:  "OBJECTIVE",
	}.render()
	want := "STANDING\n\nPROBE-A\nPROBE-B\n\nOBJECTIVE"
	if got != want {
		t.Errorf("render:\n got %q\nwant %q", got, want)
	}
}

// TestPromptLayersOmitsEmptyLayers proves an absent layer contributes no blank block, so
// a run with no standing instruction does not open the turn with stray newlines.
func TestPromptLayersOmitsEmptyLayers(t *testing.T) {
	if got := (promptLayers{input: "OBJECTIVE"}).render(); got != "OBJECTIVE" {
		t.Errorf("render with only an objective = %q", got)
	}
	if got := (promptLayers{system: "S", input: "O"}).render(); got != "S\n\nO" {
		t.Errorf("render with no probes = %q", got)
	}
}

// TestProjectionCountsEachCallOnce proves the steering metrics are not inflated by
// codex's item lifecycle. It emits item.started, item.updated, and item.completed for the
// same call, so projecting a tool event from more than one of them would count a single
// call two or three times and make the native rate meaningless.
func TestProjectionCountsEachCallOnce(t *testing.T) {
	c := NewCodex("", nil)
	const bridge = "flynn"
	lines := []string{
		`{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","server":"flynn","tool":"read","arguments":{},"status":"in_progress"}}`,
		`{"type":"item.updated","item":{"id":"i1","type":"mcp_tool_call","server":"flynn","tool":"read","arguments":{},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","server":"flynn","tool":"read","arguments":{},"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"i2","type":"command_execution","command":"ls","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"command_execution","command":"ls","status":"completed"}}`,
	}
	// The bridged call is aimed at a foreign server here, since a call on the run's own
	// bridge is counted at the waist rather than off the stream. Its lifecycle is the same,
	// so it still proves the projection does not multiply one call into three.
	var s Steering
	for _, line := range lines {
		evs, err := c.Parse([]byte(strings.ReplaceAll(line, `"server":"flynn"`, `"server":"other"`)))
		if err != nil {
			t.Fatalf("Parse(%s): %v", line, err)
		}
		for _, ev := range evs {
			s.absorbSteering(ev, bridge)
		}
	}
	if s.ForeignCalls != 1 || s.NativeCommands != 1 {
		t.Errorf("counts = %d foreign / %d native, want 1 / 1 (one call each, counted once)",
			s.ForeignCalls, s.NativeCommands)
	}
}

// TestProjectionReadsToolCallDetail proves the projection recovers the fields the probes
// and metrics rely on from a line shaped exactly as codex emits it: the item's typed
// payload is flattened into the item object alongside its id and type.
func TestProjectionReadsToolCallDetail(t *testing.T) {
	c := NewCodex("", nil)
	line := `{"type":"item.completed","item":{"id":"i1","type":"mcp_tool_call","server":"srv","tool":"write","arguments":{"nonce":"abc"},"status":"failed"}}`
	evs, err := c.Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Kind != EventBridgeCall || ev.Server != "srv" || ev.Tool != "write" || ev.Status != "failed" {
		t.Fatalf("projected %+v", ev)
	}
	if !strings.Contains(string(ev.Args), "abc") {
		t.Errorf("arguments not preserved: %s", ev.Args)
	}
	if ev.Tier != TierAttested {
		t.Errorf("the CLI's account of its own call is attested, got %v", ev.Tier)
	}
}

// TestProjectionTreatsFileChangeAsNative proves a patch the harness applied with its own
// tool counts as native tool use, not as a bridged effect.
func TestProjectionTreatsFileChangeAsNative(t *testing.T) {
	c := NewCodex("", nil)
	evs, err := c.Parse([]byte(`{"type":"item.completed","item":{"id":"i1","type":"file_change","status":"failed"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != EventNativeCommand {
		t.Fatalf("projected %+v, want a native command event", evs)
	}
	var s Steering
	s.absorbSteering(evs[0], "flynn")
	if s.NativeCommands != 1 || s.NativeDeclined != 1 {
		t.Errorf("a failed native patch: %d native / %d declined, want 1 / 1", s.NativeCommands, s.NativeDeclined)
	}
}

// TestConformanceReportSummary renders both shapes a reader sees: a clean session and a
// drifting one.
func TestConformanceReportSummary(t *testing.T) {
	clean := ConformanceReport{Results: []ProbeResult{{Name: "bridge-reachable", Passed: true, Required: true}}}
	if got := clean.Summary(); !strings.Contains(got, "1/1 probes passed") || strings.Contains(got, "drift") {
		t.Errorf("clean summary = %q", got)
	}
	if clean.Refused() {
		t.Error("a passing report must not refuse")
	}

	drifting := ConformanceReport{
		Results: []ProbeResult{
			{Name: "bridge-reachable", Passed: false, Required: true},
			{Name: "no-native-tools", Passed: false},
		},
		Steering: Steering{BridgeCalls: 1, NativeCommands: 3, NativeDeclined: 2},
	}
	got := drifting.Summary()
	for _, want := range []string{"0/2 probes passed", "bridge-reachable (required)", "no-native-tools", "75% native", "2 refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	if !drifting.Refused() {
		t.Error("a failed required probe must refuse")
	}
}

// TestAdvisoryProbeFailureDoesNotRefuse proves only a required probe refuses a session.
func TestAdvisoryProbeFailureDoesNotRefuse(t *testing.T) {
	rep := ConformanceReport{Results: []ProbeResult{
		{Name: "bridge-reachable", Passed: true, Required: true},
		{Name: "no-native-tools", Passed: false},
	}}
	if rep.Refused() {
		t.Error("an advisory probe failure must not refuse the session")
	}
	if len(rep.Failed()) != 1 {
		t.Errorf("failed probes = %d, want 1", len(rep.Failed()))
	}
}
