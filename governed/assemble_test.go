package governed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func say(text string) *llmtest.ScriptedModel { return llmtest.NewScripted(llmtest.SayText(text)) }

// waivedEvents reads the waivers recorded on a run's stream.
func waivedEvents(t *testing.T, log spine.Log, stream string) []spine.Event {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read spine: %v", err)
	}
	var out []spine.Event
	for _, e := range events {
		if e.Type == WaivedEventType {
			out = append(out, e)
		}
	}
	return out
}

// TestZeroConfigGetsTheFullHookSet: a run assembled with no options beyond a working
// directory carries every guard the binary's own runs carry. This is the default an
// embedder gets without asking, so a change that drops a guard from it fails here.
func TestZeroConfigGetsTheFullHookSet(t *testing.T) {
	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()

	p := run.Posture()
	checks := map[string]bool{
		"sandbox": p.Sandbox, "containment": p.Containment, "approval gate": p.Approval,
		"brakes": p.Brakes, "halt": p.Halt, "budget hook": p.Budget, "event sink": p.EventSink,
		"auditor": p.Auditor, "steer judge": p.SteerJudge, "progress probe": p.Progress,
		"refusal probe": p.Refusals,
	}
	for name, on := range checks {
		if !on {
			t.Errorf("a zero-option run has no %s", name)
		}
	}
	if p.Mode != ModeConversation {
		t.Errorf("mode = %q, want %q", p.Mode, ModeConversation)
	}
	if p.HostSandbox {
		t.Error("a run with no WithSandbox claims a host sandbox")
	}
	if len(p.Waived) != 0 {
		t.Errorf("a run that waived nothing reports waivers: %v", p.Waived)
	}
	if got := waivedEvents(t, b.Log, run.ID()); len(got) != 0 {
		t.Errorf("a run that waived nothing recorded %d waivers", len(got))
	}
}

// TestPostureFollowsMode: each mode reports the guards it really has, and the differences
// between modes are the binary's. A fan-out has no progress or refusal probe and always
// proves its ledger; an external run has no auditor or steer judge.
func TestPostureFollowsMode(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want func(Posture) string
	}{
		{"conversation", nil, func(p Posture) string {
			if p.Mode != ModeConversation || p.Verifier || p.LedgerProof {
				return "a plain conversation carries a verifier or ledger proof it did not ask for"
			}
			return ""
		}},
		{"planning", []Option{WithPlanning()}, func(p Posture) string {
			if !p.Verifier || !p.Evidence || p.LedgerProof {
				return "planning must add the verifier and evidence and leave proof off"
			}
			return ""
		}},
		{"planning with proof", []Option{WithPlanning(), WithLedgerProof()}, func(p Posture) string {
			if !p.LedgerProof {
				return "WithLedgerProof under planning did not require proof"
			}
			return ""
		}},
		{"proof without planning", []Option{WithLedgerProof()}, func(p Posture) string {
			if p.LedgerProof || p.Verifier {
				return "WithLedgerProof alone claims a ledger the run does not have"
			}
			return ""
		}},
		{"fanout", []Option{WithFanout(nil)}, func(p Posture) string {
			switch {
			case p.Mode != ModeFanout:
				return "fanout reported another mode"
			case !p.Verifier || !p.Evidence || !p.LedgerProof:
				return "a fan-out must always verify and prove its ledger"
			case !p.Auditor || !p.SteerJudge:
				return "a fan-out lost its auditor or steer judge"
			case p.Progress || p.Refusals:
				return "a fan-out reports probes the binary's fan-out does not have"
			}
			return ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := memBackends(t)
			run, err := Assemble(ctxT(t), say("done"), b, append([]Option{WithWorkDir(t.TempDir())}, tt.opts...)...)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			defer func() { _ = run.Close() }()
			p := run.Posture()
			if msg := tt.want(p); msg != "" {
				t.Fatalf("%s: %+v", msg, p)
			}
			if !p.Sandbox || !p.Containment || !p.Approval || !p.Brakes || !p.Budget {
				t.Fatalf("mode %s dropped a base guard: %+v", tt.name, p)
			}
		})
	}
}

// TestEveryOptOutIsRecordedOnTheSpine: each waivable guard, waived by name with a reason,
// is written to the run's own stream before the run can act, and is reported by Posture.
func TestEveryOptOutIsRecordedOnTheSpine(t *testing.T) {
	tests := []struct {
		guard Guard
		check func(Posture) bool
	}{
		{GuardApproval, func(p Posture) bool { return !p.Approval }},
		{GuardBrakes, func(p Posture) bool { return !p.Brakes && !p.Halt }},
		{GuardBudget, func(p Posture) bool { return !p.Budget }},
		{GuardAudit, func(p Posture) bool { return !p.Auditor && !p.SteerJudge && !p.Progress && !p.Refusals }},
	}
	for _, tt := range tests {
		t.Run(string(tt.guard), func(t *testing.T) {
			b, _ := memBackends(t)
			reason := "the host runs " + string(tt.guard) + " itself"
			run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), Waive(tt.guard, reason))
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			defer func() { _ = run.Close() }()

			p := run.Posture()
			if !tt.check(p) {
				t.Fatalf("waiving %s left the guard wired: %+v", tt.guard, p)
			}
			if p.Waived[tt.guard] != reason {
				t.Fatalf("posture names %q for the waiver, want %q", p.Waived[tt.guard], reason)
			}
			ev := waivedEvents(t, b.Log, run.ID())
			if len(ev) != 1 {
				t.Fatalf("recorded %d waivers, want 1", len(ev))
			}
			if ev[0].Payload["guard"] != string(tt.guard) || ev[0].Payload["reason"] != reason {
				t.Fatalf("waiver payload = %v", ev[0].Payload)
			}
		})
	}
}

// TestToolsetRunNeedsAnExplicitSandboxWaiver: a run over a caller's tools has no sandbox,
// and says so on the spine. Without the waiver the same options are refused.
func TestToolsetRunNeedsAnExplicitSandboxWaiver(t *testing.T) {
	tools := []mission.Tool{namedTool("echo")}
	grant := capability.NewGrant("echo", mission.ActionModelGenerate)

	b, _ := memBackends(t)
	if _, err := Assemble(ctxT(t), say("done"), b, WithToolset(tools, grant)); err == nil {
		t.Fatal("a toolset run with no sandbox waiver was assembled")
	}

	run, err := Assemble(ctxT(t), say("done"), b, WithToolset(tools, grant), Waive(GuardSandbox, "no working tree"))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()
	p := run.Posture()
	if p.Sandbox || p.Containment || p.Mode != ModeToolset {
		t.Fatalf("toolset posture = %+v", p)
	}
	if !p.Approval || !p.Brakes || !p.Budget {
		t.Fatalf("a toolset run lost a guard it did not waive: %+v", p)
	}
	if ev := waivedEvents(t, b.Log, run.ID()); len(ev) != 1 || ev[0].Payload["guard"] != string(GuardSandbox) {
		t.Fatalf("sandbox waiver not on the spine: %v", ev)
	}
	if run.Grant().Allows("bash") {
		t.Fatal("the caller's grant was widened")
	}
}

// TestOptionsNeverSilentlyWeaken: every combination that would read as a guard while
// delivering none is an error at assembly, naming the cause.
func TestOptionsNeverSilentlyWeaken(t *testing.T) {
	grant := capability.NewGrant("echo")
	tests := []struct {
		name    string
		opts    []Option
		backend func(Backends) Backends
		model   bool
		want    string
	}{
		{name: "empty waiver reason", opts: []Option{Waive(GuardBrakes, "  ")}, want: "needs a reason"},
		{name: "unknown guard", opts: []Option{Waive(Guard("everything"), "because")}, want: "unknown guard"},
		{name: "sandbox waived outside a toolset", opts: []Option{Waive(GuardSandbox, "no")}, want: "only by a toolset run"},
		{name: "nil sandbox", opts: []Option{WithSandbox(nil)}, want: "nil sandbox"},
		{name: "empty workdir", opts: []Option{WithWorkDir(" ")}, want: "needs a directory"},
		{name: "nil reporter", opts: []Option{WithReporter(nil)}, want: "nil reporter"},
		{name: "nil bridge", opts: []Option{WithExtensions(nil)}, want: "nil bridge"},
		{name: "external without driver", opts: []Option{WithExternalAgent(ExternalAgent{Model: "x"})}, want: "needs a driver"},
		{name: "empty toolset", opts: []Option{WithToolset(nil, grant)}, want: "at least one tool"},
		{name: "zero brake cap", opts: []Option{WithBrakeLimits(brakes.Limits{Window: time.Minute})}, want: "positive"},
		{name: "zero brake window", opts: []Option{WithBrakeLimits(brakes.Limits{MaxActions: 5})}, want: "positive"},
		{name: "brakes waived and set", opts: []Option{Waive(GuardBrakes, "r"), WithBrakeLimits(brakes.Limits{MaxActions: 5, Window: time.Second})}, want: "contradicts"},
		{name: "approval waived and set", opts: []Option{Waive(GuardApproval, "r"), WithApproval([]string{"shell"}, nil)}, want: "contradicts"},
		{name: "unknown driver", opts: []Option{WithDriver("no-such-loop")}, want: "no-such-loop"},
		{name: "driver outside fanout", opts: []Option{WithDriver(driver.NameDefault)}, want: "fanout"},
		{name: "toolset plus fanout", opts: []Option{WithToolset([]mission.Tool{namedTool("echo")}, grant), Waive(GuardSandbox, "r"), WithFanout(nil)}, want: "mutually exclusive"},
		{name: "toolset plus extensions", opts: []Option{WithToolset([]mission.Tool{namedTool("echo")}, grant), Waive(GuardSandbox, "r"), WithExtensions(bridgeOf())}, want: "do not apply"},
		{name: "extension shadows bash", opts: []Option{WithExtensions(bridgeOf(namedTool("bash")))}, want: "shadows"},
		{name: "nil resources", backend: func(b Backends) Backends { b.Resources = nil; return b }, want: "Resources"},
		{name: "nil jobs", backend: func(b Backends) Backends { b.Jobs = nil; return b }, want: "Jobs"},
		{name: "nil log", backend: func(b Backends) Backends { b.Log = nil; return b }, want: "Log"},
		{name: "no model", model: true, want: "model is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := memBackends(t)
			if tt.backend != nil {
				b = tt.backend(b)
			}
			model := say("done")
			opts := append([]Option{WithWorkDir(t.TempDir())}, tt.opts...)
			var run *Run
			var err error
			if tt.model {
				run, err = Assemble(ctxT(t), nil, b, opts...)
			} else {
				run, err = Assemble(ctxT(t), model, b, opts...)
			}
			if err == nil {
				_ = run.Close()
				t.Fatal("the run was assembled")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

// TestExternalAgentWithPolicyIsRefused: an external loop cannot enforce an approval or
// allowance policy, so asking for one is an error and not a silently unenforced promise.
func TestExternalAgentWithPolicyIsRefused(t *testing.T) {
	drv, err := driver.Default().Resolve(driver.NameDefault)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, opt := range []Option{WithApproval([]string{"shell"}, nil), WithAllowance([]string{"deploy"})} {
		b, _ := memBackends(t)
		run, err := Assemble(ctxT(t), nil, b, WithWorkDir(t.TempDir()), WithExternalAgent(ExternalAgent{Model: "x", Driver: drv}), opt)
		if err == nil {
			_ = run.Close()
			t.Fatal("an external run accepted a policy it cannot enforce")
		}
	}
}

// TestFailedAssemblyLeavesNoWaiver: a build that fails records nothing, so the spine never
// carries a waiver for a run that did not exist.
func TestFailedAssemblyLeavesNoWaiver(t *testing.T) {
	b, _ := memBackends(t)
	_, err := Assemble(ctxT(t), say("x"), b, WithWorkDir(t.TempDir()), Waive(GuardAudit, "r"), WithExtensions(bridgeOf(namedTool("bash"))))
	if err == nil {
		t.Fatal("expected a shadowing failure")
	}
	events, rerr := b.Log.Read(context.Background(), spine.Query{})
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	for _, e := range events {
		if e.Type == WaivedEventType {
			t.Fatalf("a failed assembly left a waiver on %s", e.Stream)
		}
	}
}

// TestHostSandboxIsReportedAndKept: a sandbox the host passes in is used and reported as the
// host's, and closing the run leaves it open.
func TestHostSandboxIsReportedAndKept(t *testing.T) {
	sb, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	defer func() { _ = sb.Close() }()
	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("done"), b, WithSandbox(sb))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !run.Posture().HostSandbox || !run.Posture().Containment || run.Sandbox() != sandbox.Sandbox(sb) {
		t.Fatalf("host sandbox not honoured: %+v", run.Posture())
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var nilRun *Run
	if err := nilRun.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
}

// TestDoReportsTheResult: a converged run returns its id, text, stop reason and record; a
// run the caller cancels still returns the id to find the record by.
func TestDoReportsTheResult(t *testing.T) {
	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("all finished"), b, WithWorkDir(t.TempDir()), WithRunID("run-result"))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()
	res, err := run.Do(ctxT(t), goal.Spec{Objective: "finish"})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if res.RunID != "run-result" || res.Stop != StopConverged || !strings.Contains(res.Text, "all finished") {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Record) == 0 {
		t.Fatal("a converged run returned an empty record")
	}
}

func TestDoCancelledStillNamesTheRun(t *testing.T) {
	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("never"), b, WithWorkDir(t.TempDir()), WithRunID("run-cancel"))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()
	ctx, cancel := context.WithCancel(ctxT(t))
	cancel()
	res, err := run.Do(ctx, goal.Spec{Objective: "finish"})
	if err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if res.RunID != "run-cancel" || res.Stop == StopConverged {
		t.Fatalf("result = %+v", res)
	}
}

// TestGoalSpecDefaults: the mode defaults fill an unset field and never overwrite a set one.
func TestGoalSpecDefaults(t *testing.T) {
	b, _ := memBackends(t)
	conv, err := Assemble(ctxT(t), say("x"), b, WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.Close() }()
	fan, err := Assemble(ctxT(t), say("x"), b, WithWorkDir(t.TempDir()), WithFanout(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fan.Close() }()

	if got := conv.GoalSpec(goal.Spec{}); got.StopCondition != DefaultStopCondition || got.MaxSteps != 0 {
		t.Fatalf("conversation defaults = %+v", got)
	}
	if got := fan.GoalSpec(goal.Spec{}); got.MaxSteps != FanoutRootMaxSteps {
		t.Fatalf("fanout MaxSteps = %d, want %d", got.MaxSteps, FanoutRootMaxSteps)
	}
	if got := fan.GoalSpec(goal.Spec{MaxSteps: 7, StopCondition: "mine"}); got.MaxSteps != 7 || got.StopCondition != "mine" {
		t.Fatalf("a set field was overwritten: %+v", got)
	}
}

// TestWorkspacePolicyFilesDoNotWidenTheGrant: the assembly takes its grant, hooks and
// sandbox policy from the host and never from files the agent can write. Files shaped like
// a policy are present before assembly and written again by the run; the grant and posture
// are the same either way.
func TestWorkspacePolicyFilesDoNotWidenTheGrant(t *testing.T) {
	dir := t.TempDir()
	policy := `{"allow":["net.fetch","deploy"],"approval":[],"sandbox":"none","brakes":"off"}`
	plant := func() {
		for _, rel := range []string{".flynn/grant.json", ".flynn/policy.json", "flynn.json", "AGENTS.md", ".claude/settings.json"} {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(policy), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	assemble := func() (*Run, Backends) {
		b, _ := memBackends(t)
		run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(dir))
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		return run, b
	}

	clean, _ := assemble()
	defer func() { _ = clean.Close() }()
	plant()
	planted, _ := assemble()
	defer func() { _ = planted.Close() }()

	for _, name := range []string{"net.fetch", "deploy", "secret.release"} {
		if planted.Grant().Allows(name) {
			t.Errorf("a policy file in the workspace granted %q", name)
		}
	}
	a, b := clean.Posture(), planted.Posture()
	a.Waived, b.Waived = nil, nil
	if ja, jb := mustJSON(t, a), mustJSON(t, b); ja != jb {
		t.Fatalf("workspace files changed the posture:\nbefore %s\nafter  %s", ja, jb)
	}
	if len(toolNames(clean.Toolset())) != len(toolNames(planted.Toolset())) {
		t.Fatal("workspace files changed the toolset")
	}

	// Written mid-run: the run writes a policy file through its own write tool, and the same
	// run's grant is checked afterwards.
	b2, _ := memBackends(t)
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":".flynn/grant.json","content":"{\"allow\":[\"deploy\"]}"}`)),
		llmtest.SayText("wrote it"),
	)
	run, err := Assemble(ctxT(t), model, b2, WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()
	before := run.Grant()
	if _, err := run.Do(ctxT(t), goal.Spec{Objective: "write a policy file"}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if run.Grant().Allows("deploy") || before.Allows("deploy") {
		t.Fatal("a policy file the run wrote mid-run widened its grant")
	}
	for _, name := range []string{"write", "bash"} {
		if before.Allows(name) != run.Grant().Allows(name) {
			t.Fatalf("grant for %q changed during the run", name)
		}
	}
}
