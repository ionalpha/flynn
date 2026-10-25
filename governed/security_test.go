package governed

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
)

// postureEvents reads the posture statements on a run's stream.
func postureEvents(t *testing.T, log spine.Log, stream string) []spine.Event {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read spine: %v", err)
	}
	var out []spine.Event
	for _, e := range events {
		if e.Type == PostureEventType {
			out = append(out, e)
		}
	}
	return out
}

// TestRunRecordStatesWiredGuards: the run's own stream says which guards are in force, so
// a sealed record carries the posture and a reader does not take the host's word for it.
func TestRunRecordStatesWiredGuards(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want map[string]bool
	}{
		{"zero options", nil, map[string]bool{
			"sandbox": true, "containment": true, "approval": true, "brakes": true,
			"budget": true, "halt": true, "eventSink": true, "auditor": true,
		}},
		{"budget waived", []Option{Waive(GuardBudget, "the host meters spend")}, map[string]bool{
			"sandbox": true, "approval": true, "brakes": true, "budget": false,
		}},
		{"brakes waived", []Option{Waive(GuardBrakes, "the host halts runs")}, map[string]bool{
			"brakes": false, "halt": false, "budget": true,
		}},
		{"audit waived", []Option{Waive(GuardAudit, "the host audits")}, map[string]bool{
			"auditor": false, "steerJudge": false, "progress": false, "refusals": false, "sandbox": true,
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
			if _, err := run.Do(ctxT(t), goal.Spec{Objective: "answer"}); err != nil {
				t.Fatalf("run: %v", err)
			}

			ev := postureEvents(t, b.Log, run.ID())
			if len(ev) != 1 {
				t.Fatalf("recorded %d posture statements, want 1", len(ev))
			}
			for key, want := range tt.want {
				if got, _ := ev[0].Payload[key].(bool); got != want {
					t.Errorf("record says %s = %v, want %v (payload %v)", key, got, want, ev[0].Payload)
				}
			}
			waived, _ := ev[0].Payload["waived"].(map[string]any)
			if len(waived) != len(run.Posture().Waived) {
				t.Errorf("record names %d waivers, posture names %d", len(waived), len(run.Posture().Waived))
			}
		})
	}
}

// TestRecordedPostureMatchesThePostureReported: the record and the in-process report are
// one claim; a field present in one and absent from the other is a way to say two things.
func TestRecordedPostureMatchesThePostureReported(t *testing.T) {
	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), WithApproval([]string{"write", "bash"}, nil), WithPlanning())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()
	// The planning pass needs more model turns than the one scripted, so the goal stalls;
	// the posture is on the record from submission whatever follows.
	_, _ = run.Do(ctxT(t), goal.Spec{Objective: "answer"})

	ev := postureEvents(t, b.Log, run.ID())
	if len(ev) != 1 {
		t.Fatalf("recorded %d posture statements, want 1", len(ev))
	}
	p := run.Posture()
	flags := map[string]bool{
		"sandbox": p.Sandbox, "hostSandbox": p.HostSandbox, "containment": p.Containment,
		"approval": p.Approval, "allowance": p.Allowance, "brakes": p.Brakes, "budget": p.Budget,
		"halt": p.Halt, "eventSink": p.EventSink, "verifier": p.Verifier, "evidence": p.Evidence,
		"ledgerProof": p.LedgerProof, "auditor": p.Auditor, "steerJudge": p.SteerJudge,
		"progress": p.Progress, "refusals": p.Refusals,
	}
	for key, want := range flags {
		if got, _ := ev[0].Payload[key].(bool); got != want {
			t.Errorf("record says %s = %v, posture says %v", key, got, want)
		}
	}
	var actions []string
	raw, _ := json.Marshal(ev[0].Payload["approvalActions"])
	_ = json.Unmarshal(raw, &actions)
	if strings.Join(actions, ",") != strings.Join(p.ApprovalActions, ",") || len(actions) != 2 {
		t.Errorf("record lists approval actions %v, posture %v", actions, p.ApprovalActions)
	}
}

// TestAssemblyRefusesAnUnrestrictedGrant: the grant a toolset run is given must name its
// actions. An unrestricted grant reads as a policy and admits everything, so it is refused;
// the zero grant is accepted and admits nothing.
func TestAssemblyRefusesAnUnrestrictedGrant(t *testing.T) {
	tools := []mission.Tool{namedTool("echo")}
	b, _ := memBackends(t)

	run, err := Assemble(ctxT(t), say("done"), b, WithToolset(tools, capability.AllowAll()), Waive(GuardSandbox, "no tree"))
	if err == nil {
		_ = run.Close()
		t.Fatal("a toolset run with an unrestricted grant was assembled")
	}
	if !strings.Contains(err.Error(), "unrestricted") {
		t.Fatalf("error %q does not name the unrestricted grant", err)
	}

	run, err = Assemble(ctxT(t), say("done"), b, WithToolset(tools, capability.Grant{}), Waive(GuardSandbox, "no tree"))
	if err != nil {
		t.Fatalf("a zero grant should be accepted as deny-all: %v", err)
	}
	defer func() { _ = run.Close() }()
	if run.Grant().Allows("echo") || run.Grant().Allows(mission.ActionModelGenerate) || run.Grant().Unrestricted() {
		t.Fatal("the zero grant admitted an action")
	}
}

// TestNativeRunBindsAGrant: every mode that builds its own tools binds a grant of exactly
// those tools plus the actions the run needs, so the waist is never consulted with no grant
// on the context (which the admitter would read as permissive).
func TestNativeRunBindsAGrant(t *testing.T) {
	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()

	g := run.Grant()
	if g.Unrestricted() || len(g.Actions()) == 0 {
		t.Fatalf("grant = %v unrestricted=%v, want a named, non-empty grant", g.Actions(), g.Unrestricted())
	}
	for _, name := range toolNames(run.Toolset()) {
		if !g.Allows(name) {
			t.Errorf("the run offers %q and does not grant it", name)
		}
	}
	for _, name := range []string{"net.fetch", "deploy", "spawn"} {
		if g.Allows(name) {
			t.Errorf("a plain conversation was granted %q", name)
		}
	}
}

// TestNoWithoutOptionsExported: the package has no exported function whose name offers to
// switch a guard off. The only way to run with less is Waive, which demands a reason and
// writes it down; a convenience option for turning a guard off would bypass that.
func TestNoWithoutOptionsExported(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"Without", "No", "Disable", "Skip", "Unsafe", "Insecure", "Bypass", "Unguarded", "Ungoverned", "Allow"}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range src.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			for _, prefix := range banned {
				// A prefix counts only at a word boundary, so Noticewriter-style names that merely
				// begin with the letters are not flagged.
				rest, found := strings.CutPrefix(fn.Name.Name, prefix)
				if found && (rest == "" || rest[0] >= 'A' && rest[0] <= 'Z') {
					t.Errorf("%s exports %s: a guard is given up only through Waive", f, fn.Name.Name)
				}
			}
		}
	}
}

// errPrompter fails every request, as a prompter whose terminal has gone away would.
type errPrompter struct{}

func (errPrompter) Prompt(context.Context, mission.ApprovalRequest) (mission.ApprovalDecision, error) {
	return mission.ApprovalDecision{Allow: true}, errors.New("terminal closed")
}

// TestPrompterErrorDenies: a prompter that errors does not approve, even when the decision
// it returned alongside the error says Allow. The write the model asked for never happens.
func TestPrompterErrorDenies(t *testing.T) {
	dir := t.TempDir()
	b, _ := memBackends(t)
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"marker.txt","content":"x"}`)),
		llmtest.SayText("done"),
	)
	run, err := Assemble(ctxT(t), model, b, WithWorkDir(dir), WithApproval([]string{"write"}, errPrompter{}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()

	_, _ = run.Do(ctxT(t), goal.Spec{Objective: "write the marker"})

	if _, err := os.Stat(filepath.Join(dir, "marker.txt")); err == nil {
		t.Fatal("a write went through after the prompter failed")
	}
}

// TestApprovalWithoutAPrompterDenies: a policy naming an action and nothing able to answer
// is a refusal, not a pass-through.
func TestApprovalWithoutAPrompterDenies(t *testing.T) {
	dir := t.TempDir()
	b, _ := memBackends(t)
	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"marker.txt","content":"x"}`)),
		llmtest.SayText("done"),
	)
	run, err := Assemble(ctxT(t), model, b, WithWorkDir(dir), WithApproval([]string{"write"}, nil))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()

	_, _ = run.Do(ctxT(t), goal.Spec{Objective: "write the marker"})

	if _, err := os.Stat(filepath.Join(dir, "marker.txt")); err == nil {
		t.Fatal("a write needing approval ran with nobody to approve it")
	}
}

// closeCounter records how many times it was closed.
type closeCounter struct{ n int }

func (c *closeCounter) Close() error { c.n++; return nil }

// TestRunClosesOnlyWhatItOwns: the sandbox a run built is closed with the run, once; a
// sandbox the host passed in is the host's to close and the run leaves it alone.
func TestRunClosesOnlyWhatItOwns(t *testing.T) {
	owned := &closeCounter{}
	r := &Run{p: &parts{owned: owned}}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if owned.n != 1 {
		t.Fatalf("owned resource closed %d times, want 1", owned.n)
	}

	hosted := &Run{p: &parts{}}
	if err := hosted.Close(); err != nil {
		t.Fatalf("close hosted: %v", err)
	}
}

// streamTypes lists the event types on a run's stream in order.
func streamTypes(t *testing.T, log spine.Log, stream string) []string {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

// TestPostureIsOnTheRecordBeforeAnythingStartsTheRun: Assemble returns with the waivers and
// the posture already on the stream, so a goal started by any path (Submit, Resume, or a
// host driving Runtime itself, as a served goal does) has the statement ahead of its first
// action. Checked before any start path is called, since none can run earlier than the
// assembly, and again after Do to pin the order against the session's own events.
func TestPostureIsOnTheRecordBeforeAnythingStartsTheRun(t *testing.T) {
	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), Waive(GuardBudget, "the host meters spend"))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = run.Close() }()

	got := streamTypes(t, b.Log, run.ID())
	if len(got) != 2 || got[0] != WaivedEventType || got[1] != PostureEventType {
		t.Fatalf("stream after Assemble = %v, want the waiver and then the posture", got)
	}

	if _, err := run.Do(ctxT(t), goal.Spec{Objective: "answer"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	got = streamTypes(t, b.Log, run.ID())
	started := slices.Index(got, string(session.KindSessionStarted))
	if started < 2 || got[1] != PostureEventType {
		t.Fatalf("stream = %v, want the posture ahead of %s", got, session.KindSessionStarted)
	}
}

// TestResumeStatesThePostureAgain: a resumed run is a new assembly and says how it was
// wired, so a continuation cannot quietly carry fewer guards than the run it continues.
func TestResumeStatesThePostureAgain(t *testing.T) {
	b, _ := memBackends(t)
	first, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = first.Close() }()
	if _, err := first.Do(ctxT(t), goal.Spec{Objective: "answer"}); err != nil {
		t.Fatalf("run: %v", err)
	}

	second, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), WithRunID(first.ID()),
		Waive(GuardBudget, "the host meters spend"))
	if err != nil {
		t.Fatalf("assemble the resumed run: %v", err)
	}
	defer func() { _ = second.Close() }()
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	if err := second.Resume(ctx, first.ID()); err != nil {
		t.Fatalf("resume: %v", err)
	}

	ev := postureEvents(t, b.Log, first.ID())
	if len(ev) != 2 {
		t.Fatalf("recorded %d posture statements across the run and its resumption, want 2", len(ev))
	}
	if got, _ := ev[0].Payload["budget"].(bool); !got {
		t.Error("the original run's statement lost its budget")
	}
	if got, _ := ev[1].Payload["budget"].(bool); got {
		t.Error("the resumed run's statement says the budget is in force, and it was waived")
	}
}

// bareSandbox hides the containment declaration of the sandbox it wraps, as a host
// sandbox that declares no tier would, and counts closes.
type bareSandbox struct {
	sandbox.Sandbox
	closed int
}

func (s *bareSandbox) Close() error { s.closed++; return nil }

// TestHostSandboxWithoutATierStillGetsTheContainmentGate: a host sandbox that declares no
// containment is the weakest tier, and the gate is installed anyway so the tier is
// enforced against what each action requires and not skipped for want of a declaration.
func TestHostSandboxWithoutATierStillGetsTheContainmentGate(t *testing.T) {
	inner, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	defer func() { _ = inner.Close() }()
	sb := &bareSandbox{Sandbox: inner}
	if _, declared := any(sb).(sandbox.Contained); declared {
		t.Fatal("the test sandbox declares a tier")
	}

	b, _ := memBackends(t)
	run, err := Assemble(ctxT(t), say("done"), b, WithSandbox(sb))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	p := run.Posture()
	if !p.HostSandbox || !p.Containment {
		t.Fatalf("posture = %+v, want a host sandbox with the containment gate wired", p)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if sb.closed != 0 {
		t.Fatalf("the run closed a sandbox the host owns, %d times", sb.closed)
	}
}

// TestRunLevelCheckersAreContained: the auditor and the verifier run model-authored
// commands, so the dispatcher they share must refuse semi-trusted work on a sandbox that
// declares no tier. Without the gate such a command would run unconfined on a host that
// cannot isolate it.
func TestRunLevelCheckersAreContained(t *testing.T) {
	inner, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	defer func() { _ = inner.Close() }()
	d := dispatch.New(contained(&bareSandbox{Sandbox: inner})...)

	tests := []struct {
		name  string
		trust sandbox.Trust
		runs  bool
	}{
		{"trusted work runs at any tier", sandbox.TrustTrusted, true},
		{"model-authored work is refused", sandbox.TrustSemi, false},
		{"external work is refused", sandbox.TrustUntrusted, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			err := d.Govern(ctxT(t), dispatch.Action{Name: "check", Trust: tc.trust},
				func(context.Context) (dispatch.Metering, error) { ran = true; return dispatch.Metering{}, nil })
			if ran != tc.runs || (err == nil) != tc.runs {
				t.Fatalf("ran = %v, err = %v, want ran = %v", ran, err, tc.runs)
			}
		})
	}
}
