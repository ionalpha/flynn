package agent

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/archetypes"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/tools"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestGoalResultReportsTheRunAndItsPosture: a zero-policy Config is the governed run, and
// the result says so. A change that lets Config assemble a run with a guard off by default
// fails on the posture here.
func TestGoalResultReportsTheRunAndItsPosture(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir()})

	res, err := a.runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("all done")), "say so")
	if err != nil {
		t.Fatalf("runGoalResult: %v", err)
	}

	if res.RunID == "" || res.Stop != governed.StopConverged || !strings.Contains(res.Text, "all done") {
		t.Fatalf("result = %+v, want a converged run with an id and the answer", res)
	}
	if len(res.Record) == 0 {
		t.Fatal("the result carries no record")
	}
	p := res.Posture
	for name, on := range map[string]bool{
		"sandbox": p.Sandbox, "containment": p.Containment, "approval": p.Approval,
		"brakes": p.Brakes, "budget": p.Budget, "halt": p.Halt, "event sink": p.EventSink,
	} {
		if !on {
			t.Errorf("a zero-policy Config left %s off", name)
		}
	}
	if len(p.Waived) != 0 {
		t.Errorf("waived = %v, want none without a Waivers entry", p.Waived)
	}
}

// TestConfigBackendsAreAllOrNone: a durable store with a volatile record, or the reverse,
// reads as recorded and is not, so a partial set is refused rather than completed.
func TestConfigBackendsAreAllOrNone(t *testing.T) {
	b, err := governed.MemoryBackends()
	if err != nil {
		t.Fatalf("backends: %v", err)
	}
	tests := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"none", Config{}, true},
		{"all", Config{Resources: b.Resources, Jobs: b.Jobs, Log: b.Log}, true},
		{"store only", Config{Resources: b.Resources}, false},
		{"log only", Config{Log: b.Log}, false},
		{"jobs and log", Config{Jobs: b.Jobs, Log: b.Log}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.WorkDir = t.TempDir()
			_, err := New(tc.cfg).runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("ok")), "go")
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want success=%v", err, tc.ok)
			}
		})
	}
}

// TestHostBackendsHoldTheRecord: the run lands on the backends the host passed, under the
// id the result reports, so a host that supplies a durable log finds the run there.
func TestHostBackendsHoldTheRecord(t *testing.T) {
	b, err := governed.MemoryBackends()
	if err != nil {
		t.Fatalf("backends: %v", err)
	}
	a := New(Config{WorkDir: t.TempDir(), Resources: b.Resources, Jobs: b.Jobs, Log: b.Log})

	res, err := a.runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("recorded")), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	events, err := b.Log.Read(context.Background(), spine.Query{Stream: res.RunID})
	if err != nil || len(events) == 0 {
		t.Fatalf("host log has %d events for %s (err %v), want the run's record", len(events), res.RunID, err)
	}
}

// TestEveryConfigOptOutIsNamedAndRecorded: the only way Config runs with less is a Waivers
// entry, which the posture reports and the spine records before the run acts.
func TestEveryConfigOptOutIsNamedAndRecorded(t *testing.T) {
	const reason = "the host owns confinement for this tool set"
	dir := t.TempDir()
	sb, err := sandbox.NewLocal(dir, sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	b, err := governed.MemoryBackends()
	if err != nil {
		t.Fatalf("backends: %v", err)
	}
	a := New(Config{
		WorkDir: dir, Resources: b.Resources, Jobs: b.Jobs, Log: b.Log,
		Policy: &Policy{
			Tools:   tools.New(sb).Tools(),
			Waivers: []governed.Waiver{{Guard: governed.GuardSandbox, Reason: reason}},
		},
	})

	res, err := a.runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("ok")), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Posture.Sandbox || res.Posture.Waived[governed.GuardSandbox] != reason {
		t.Fatalf("posture = %+v, want the sandbox off and waived with its reason", res.Posture)
	}
	events, err := b.Log.Read(context.Background(), spine.Query{Stream: res.RunID})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !slices.ContainsFunc(events, func(e spine.Event) bool { return e.Type == governed.WaivedEventType }) {
		t.Fatal("the waiver is not on the run's stream")
	}
}

// TestConfigRefusesWhatItCannotGovern: each combination would claim a guard the run does
// not have, and fails before anything runs.
func TestConfigRefusesWhatItCannotGovern(t *testing.T) {
	sb, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	ext := &governed.ExternalAgent{
		Model:  "gpt-5-codex",
		Driver: externagent.NewDriver(externagent.NewCodex("", nil), nil, t.TempDir()),
	}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"host tools without a sandbox waiver", Config{Policy: &Policy{Tools: tools.New(sb).Tools()}}, "sandbox"},
		{"waiver without a reason", Config{Policy: &Policy{Waivers: []governed.Waiver{{Guard: governed.GuardBrakes}}}}, "reason"},
		{"waiver of an unknown guard", Config{Policy: &Policy{Waivers: []governed.Waiver{{Guard: "everything", Reason: "no"}}}}, "unknown guard"},
		{"external agent with approval policy", Config{ExternalAgent: ext, Policy: &Policy{RequireApproval: []string{"bash"}}}, "external"},
		{"external agent with host tools", Config{ExternalAgent: ext, Policy: &Policy{Tools: tools.New(sb).Tools()}}, "external"},
		{"external agent with an archetype", Config{ExternalAgent: ext, Agent: &archetypes.Spec{Capabilities: []string{"read"}}}, "Config.Agent"},
		{"host tools with a driver name", Config{Driver: "single-shot", Policy: &Policy{
			Tools:   tools.New(sb).Tools(),
			Waivers: []governed.Waiver{{Guard: governed.GuardSandbox, Reason: "test"}},
		}}, "Config.Driver"},
		{"external agent with a driver name", Config{ExternalAgent: ext, Driver: "single-shot"}, "Config.Driver"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.WorkDir = t.TempDir()
			_, err := New(tc.cfg).runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("unreachable")), "go")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// TestConfigApprovalPolicyReachesTheRun: the listed actions are on the run's posture. The
// refusal itself is the governed package's to prove.
func TestConfigApprovalPolicyReachesTheRun(t *testing.T) {
	a := New(Config{WorkDir: t.TempDir(), Policy: &Policy{RequireApproval: []string{"bash"}}})

	res, err := a.runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("ok")), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !res.Posture.Approval || !slices.Contains(res.Posture.ApprovalActions, "bash") {
		t.Fatalf("posture = %+v, want approval on and bash listed", res.Posture)
	}
}

// TestConfigStaysComparable: a host may compare Configs or key a map by one, and a slice
// field breaks that at compile time for every such host. The list-valued settings sit
// behind Policy for this reason.
func TestConfigStaysComparable(t *testing.T) {
	if !reflect.TypeOf(Config{}).Comparable() {
		t.Fatal("Config is not comparable; a list-valued field belongs in Policy")
	}
	p := &Policy{RequireApproval: []string{"bash"}}
	if (Config{Policy: p}) != (Config{Policy: p}) {
		t.Fatal("two Configs sharing a Policy compare unequal")
	}
}

// closeSpy counts closes of the sandbox it wraps.
type closeSpy struct {
	sandbox.Sandbox
	closed int
}

func (s *closeSpy) Close() error { s.closed++; return nil }

// TestFacadeLeavesAHostSandboxOpen: a sandbox the host put in Config is the host's to close,
// and a run that finishes with it still open lets the host reuse it for the next goal.
func TestFacadeLeavesAHostSandboxOpen(t *testing.T) {
	inner, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	defer func() { _ = inner.Close() }()
	spy := &closeSpy{Sandbox: inner}

	a := New(Config{Sandbox: spy})
	res, err := a.runGoalResult(ctxT(t), llmtest.NewScripted(llmtest.SayText("done")), "say so")
	if err != nil {
		t.Fatalf("runGoalResult: %v", err)
	}
	if !res.Posture.HostSandbox || !res.Posture.Containment {
		t.Errorf("posture = %+v, want the host sandbox reported with the containment gate", res.Posture)
	}
	if spy.closed != 0 {
		t.Fatalf("the facade closed the host's sandbox %d times", spy.closed)
	}
}

// TestConfigDocStatesHowAGuardIsGivenUp: Config is the facade an embedder reads first, so its
// doc must say that no zero value removes a guard and name Waivers as the one opt-out. A
// rewrite that drops either leaves the reader to guess.
func TestConfigDocStatesHowAGuardIsGivenUp(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "agent.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var doc string
	for _, d := range src.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.TYPE {
			for _, s := range g.Specs {
				if ts := s.(*ast.TypeSpec); ts.Name.Name == "Config" {
					doc = g.Doc.Text()
				}
			}
		}
	}
	for _, want := range []string{"zero value removes a guard", "Waivers"} {
		if !strings.Contains(strings.Join(strings.Fields(doc), " "), want) {
			t.Errorf("the Config doc does not say %q", want)
		}
	}
}
