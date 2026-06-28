package flow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeExecer scripts a command result and records the last command it was asked to run,
// so a test can assert both the outcome and that the command was templated.
type fakeExecer struct {
	res     ExecResult
	err     error
	lastCmd string
}

func (e *fakeExecer) Exec(_ context.Context, req ExecRequest) (ExecResult, error) {
	e.lastCmd = req.Command
	return e.res, e.err
}

// fakeResolver scripts a resolved path (or an error) for a named dependency.
type fakeResolver struct {
	path     string
	err      error
	lastName string
}

func (r *fakeResolver) Resolve(_ context.Context, name string) (string, error) {
	r.lastName = name
	return r.path, r.err
}

// fakeConfirmer scripts a confirmation outcome and records the message it was shown.
type fakeConfirmer struct {
	err     error
	lastMsg string
}

func (c *fakeConfirmer) Confirm(_ context.Context, message string) error {
	c.lastMsg = message
	return c.err
}

func runFlow(t *testing.T, src string, config map[string]any, opts ...Option) (any, error) {
	t.Helper()
	f, err := Decode([]byte(src))
	if err != nil {
		return nil, err
	}
	return New(opts...).Run(context.Background(), f, config)
}

// TestAssertPasses proves a true assertion does not stop the flow.
func TestAssertPasses(t *testing.T) {
	out, err := runFlow(t, `{"steps":[
		{"op":"assert","assert":{"that":"config.n == 1"}},
		{"op":"return","return":{"value":"ok"}}
	]}`, map[string]any{"n": float64(1)})
	if err != nil || out != "ok" {
		t.Fatalf("expected ok, got %v err=%v", out, err)
	}
}

// TestAssertFailsWithRenderedMessage proves a false assertion stops the flow with its
// templated message.
func TestAssertFailsWithRenderedMessage(t *testing.T) {
	_, err := runFlow(t, `{"steps":[
		{"op":"assert","assert":{"that":"config.n == 2","message":"want 2 got {{config.n}}"}}
	]}`, map[string]any{"n": float64(1)})
	if err == nil || !strings.Contains(err.Error(), "want 2 got 1") {
		t.Fatalf("expected the rendered assertion message, got %v", err)
	}
}

// TestExecRecordsOutput proves an exec step runs the templated command and exposes its
// exit code and output to later steps.
func TestExecRecordsOutput(t *testing.T) {
	ex := &fakeExecer{res: ExecResult{ExitCode: 0, Output: "hello"}}
	out, err := runFlow(t, `{"steps":[
		{"id":"r","op":"exec","exec":{"command":"echo {{config.msg}}"}},
		{"op":"return","return":{"value":"{{steps.r.output}}"}}
	]}`, map[string]any{"msg": "hi"}, WithExec(ex))
	if err != nil || out != "hello" {
		t.Fatalf("expected hello, got %v err=%v", out, err)
	}
	if ex.lastCmd != "echo hi" {
		t.Fatalf("command not templated: %q", ex.lastCmd)
	}
}

// TestExecNonzeroFails proves a command that exits nonzero fails the flow by default, and
// that the failure carries the command and its output so the cause is diagnosable rather
// than an opaque exit code.
func TestExecNonzeroFails(t *testing.T) {
	ex := &fakeExecer{res: ExecResult{ExitCode: 1, Output: "boom: it broke"}}
	_, err := runFlow(t, `{"steps":[
		{"op":"exec","exec":{"command":"do-the-thing"}}
	]}`, nil, WithExec(ex))
	if err == nil {
		t.Fatal("expected a nonzero exit to fail the flow")
	}
	if !strings.Contains(err.Error(), "do-the-thing") {
		t.Errorf("error should name the failed command, got: %v", err)
	}
	if !strings.Contains(err.Error(), "boom: it broke") {
		t.Errorf("error should carry the command output, got: %v", err)
	}
}

// recordingObserver captures every step event it is shown, so a test can assert what the
// interpreter reported and in what order.
type recordingObserver struct{ events []StepEvent }

func (o *recordingObserver) Step(ev StepEvent) { o.events = append(o.events, ev) }

// TestObserverReportsExecSteps proves the interpreter reports each command step to an
// observer as it begins and ends, carrying the rendered command, exit code, and output, so
// a host can show progress while a flow runs.
func TestObserverReportsExecSteps(t *testing.T) {
	ex := &fakeExecer{res: ExecResult{ExitCode: 0, Output: "done"}}
	obs := &recordingObserver{}
	_, err := runFlow(t, `{"steps":[
		{"op":"exec","exec":{"command":"deploy {{config.app}}"}},
		{"op":"return","return":{"value":"ok"}}
	]}`, map[string]any{"app": "myapp"}, WithExec(ex), WithObserver(obs))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(obs.events) != 2 {
		t.Fatalf("expected begin+end events, got %d: %+v", len(obs.events), obs.events)
	}
	begin, end := obs.events[0], obs.events[1]
	if begin.Phase != StepBegin || begin.Op != OpExec || begin.Detail != "deploy myapp" {
		t.Errorf("unexpected begin event: %+v", begin)
	}
	if end.Phase != StepEnd || end.ExitCode != 0 || end.Output != "done" || end.Err != nil {
		t.Errorf("unexpected end event: %+v", end)
	}
}

// TestObserverReportsExecFailure proves a failing command's end event carries the error, so
// a host can mark the step failed while the run's own error still reports the cause.
func TestObserverReportsExecFailure(t *testing.T) {
	ex := &fakeExecer{res: ExecResult{ExitCode: 2, Output: "nope"}}
	obs := &recordingObserver{}
	_, err := runFlow(t, `{"steps":[
		{"op":"exec","exec":{"command":"broken"}}
	]}`, nil, WithExec(ex), WithObserver(obs))
	if err == nil {
		t.Fatal("expected the flow to fail")
	}
	if len(obs.events) != 2 || obs.events[1].Phase != StepEnd {
		t.Fatalf("expected a begin and an end event, got: %+v", obs.events)
	}
	if obs.events[1].ExitCode != 2 {
		t.Errorf("end event should carry the exit code, got: %+v", obs.events[1])
	}
}

// TestExecAllowNonzero proves AllowNonzero lets the flow continue and inspect the exit
// code instead of failing.
func TestExecAllowNonzero(t *testing.T) {
	ex := &fakeExecer{res: ExecResult{ExitCode: 3, Output: "nope"}}
	out, err := runFlow(t, `{"steps":[
		{"id":"r","op":"exec","exec":{"command":"thing","allowNonzero":true}},
		{"op":"return","return":{"value":"{{steps.r.exitCode}}"}}
	]}`, nil, WithExec(ex))
	if err != nil {
		t.Fatalf("allowNonzero should not fail: %v", err)
	}
	if n, ok := out.(float64); !ok || n != 3 {
		t.Fatalf("expected exit code 3, got %v", out)
	}
}

// TestExecNoRunnerFailsClosed proves an exec step with no runner configured fails rather
// than reaching a process some other way.
func TestExecNoRunnerFailsClosed(t *testing.T) {
	_, err := runFlow(t, `{"steps":[{"op":"exec","exec":{"command":"x"}}]}`, nil)
	if err == nil {
		t.Fatal("an exec step without a runner must fail closed")
	}
}

// TestDependencyBindsPath proves a dependency step resolves the program and exposes its
// path to later steps.
func TestDependencyBindsPath(t *testing.T) {
	res := &fakeResolver{path: "/data/deps/flyctl/0.4.61/flyctl"}
	out, err := runFlow(t, `{"steps":[
		{"id":"d","op":"dependency","dependency":{"name":"flyctl"}},
		{"op":"return","return":{"value":"{{steps.d.path}}"}}
	]}`, nil, WithDependencies(res))
	if err != nil || out != "/data/deps/flyctl/0.4.61/flyctl" {
		t.Fatalf("expected the resolved path, got %v err=%v", out, err)
	}
	if res.lastName != "flyctl" {
		t.Fatalf("resolver got the wrong name: %q", res.lastName)
	}
}

// TestDependencyResolverErrorPropagates proves a resolver failure stops the flow.
func TestDependencyResolverErrorPropagates(t *testing.T) {
	res := &fakeResolver{err: errors.New("no build for plan9")}
	_, err := runFlow(t, `{"steps":[{"op":"dependency","dependency":{"name":"flyctl"}}]}`, nil, WithDependencies(res))
	if err == nil {
		t.Fatal("expected the resolver error to stop the flow")
	}
}

// TestDependencyNoResolverFailsClosed proves a dependency step with no resolver fails.
func TestDependencyNoResolverFailsClosed(t *testing.T) {
	_, err := runFlow(t, `{"steps":[{"op":"dependency","dependency":{"name":"x"}}]}`, nil)
	if err == nil {
		t.Fatal("a dependency step without a resolver must fail closed")
	}
}

// TestConfirmProceeds proves an approved confirmation lets the flow continue, and that the
// message is templated and shown.
func TestConfirmProceeds(t *testing.T) {
	c := &fakeConfirmer{}
	out, err := runFlow(t, `{"steps":[
		{"op":"confirm","confirm":{"message":"Log in to {{config.svc}}? Press Enter."}},
		{"op":"return","return":{"value":"continued"}}
	]}`, map[string]any{"svc": "Fly"}, WithConfirm(c))
	if err != nil || out != "continued" {
		t.Fatalf("expected the flow to continue, got %v err=%v", out, err)
	}
	if c.lastMsg != "Log in to Fly? Press Enter." {
		t.Fatalf("confirm message not templated: %q", c.lastMsg)
	}
}

// TestConfirmDeclinesStopsFlow proves a declined (or unanswerable) confirmation stops the
// flow rather than proceeding without consent.
func TestConfirmDeclinesStopsFlow(t *testing.T) {
	c := &fakeConfirmer{err: errors.New("cancelled by operator")}
	_, err := runFlow(t, `{"steps":[
		{"op":"confirm","confirm":{"message":"proceed?"}},
		{"op":"return","return":{"value":"continued"}}
	]}`, nil, WithConfirm(c))
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected a declined confirmation to stop the flow, got %v", err)
	}
}

// TestConfirmNoPrompterFailsClosed proves a confirm step with no prompter fails rather than
// silently proceeding.
func TestConfirmNoPrompterFailsClosed(t *testing.T) {
	_, err := runFlow(t, `{"steps":[{"op":"confirm","confirm":{"message":"ok?"}}]}`, nil)
	if err == nil {
		t.Fatal("a confirm step without a prompter must fail closed")
	}
}

// TestNewOpsValidation proves the new ops are admitted only when well-formed: an assert
// with no condition, an exec with no command, and a dependency with no name are rejected
// at decode.
func TestNewOpsValidation(t *testing.T) {
	bad := []string{
		`{"steps":[{"op":"assert","assert":{}}]}`,
		`{"steps":[{"op":"exec","exec":{}}]}`,
		`{"steps":[{"op":"dependency","dependency":{}}]}`,
		`{"steps":[{"op":"confirm","confirm":{}}]}`,
	}
	for _, src := range bad {
		if _, err := Decode([]byte(src)); err == nil {
			t.Fatalf("expected %s to be rejected", src)
		}
	}
	// A well-formed flow using all three admits.
	good := `{"steps":[
		{"op":"dependency","dependency":{"name":"flyctl"}},
		{"op":"exec","exec":{"command":"flyctl version"}},
		{"op":"assert","assert":{"that":"true","message":"ok"}},
		{"op":"return","return":{"value":"done"}}
	]}`
	if _, err := Decode([]byte(good)); err != nil {
		t.Fatalf("a well-formed flow must admit: %v", err)
	}
}
