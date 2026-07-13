package flow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

// TestValidateAcceptsAWellFormedFlow proves static admission passes a flow whose every
// step, expression, and template is well formed, without running it.
func TestValidateAcceptsAWellFormedFlow(t *testing.T) {
	f := mustDecode(t, `{"steps":[
		{"id":"pick","op":"transform","transform":{"value":"config.n + 1"}},
		{"op":"return","return":{"value":"{{steps.pick}}"}}
	]}`)
	if err := f.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestValidateRejectsStructure proves Validate is the same admission gate Decode applies:
// a flow that is structurally wrong is refused before anything runs, and the refusal names
// the step so an author can find it.
func TestValidateRejectsStructure(t *testing.T) {
	cases := []struct {
		name string
		flow Flow
		want string
	}{
		{
			name: "no steps",
			flow: Flow{},
			want: "at least one step",
		},
		{
			name: "op with no action block",
			flow: Flow{Steps: []Step{{ID: "a", Op: OpHTTP}}},
			want: "exactly one action block",
		},
		{
			name: "action block does not match the op",
			flow: Flow{Steps: []Step{{ID: "a", Op: OpHTTP, Call: &CallAction{Tool: "t"}}}},
			want: "exactly one action block",
		},
		{
			name: "two action blocks",
			flow: Flow{Steps: []Step{{
				ID:        "a",
				Op:        OpTransform,
				Transform: &TransformAction{Value: "1"},
				Call:      &CallAction{Tool: "t"},
			}}},
			want: "found 2",
		},
		{
			name: "unknown op",
			flow: Flow{Steps: []Step{{Op: Op("exfiltrate")}}},
			want: "exactly one action block",
		},
		{
			name: "duplicate ids",
			flow: Flow{Steps: []Step{
				{ID: "a", Op: OpTransform, Transform: &TransformAction{Value: "1"}},
				{ID: "a", Op: OpTransform, Transform: &TransformAction{Value: "2"}},
			}},
			want: "duplicate step id a",
		},
		{
			name: "duplicate id inside a branch",
			flow: Flow{Steps: []Step{
				{ID: "a", Op: OpTransform, Transform: &TransformAction{Value: "1"}},
				{Op: OpCondition, Condition: &ConditionAction{If: "true", Then: []Step{
					{ID: "a", Op: OpTransform, Transform: &TransformAction{Value: "2"}},
				}}},
			}},
			want: "duplicate step id a",
		},
		{
			name: "a bad expression inside a loop body",
			flow: Flow{Steps: []Step{
				{Op: OpLoop, Loop: &LoopAction{Count: 1, Body: []Step{
					{Op: OpTransform, Transform: &TransformAction{Value: "1 +"}},
				}}},
			}},
			want: "flow:",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.flow.Validate()
			if err == nil {
				t.Fatal("expected the flow to be refused")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected %q, got %v", c.want, err)
			}
			assertTerminal(t, err)
		})
	}
}

// TestUnknownOpNamesTheStepByOp proves the refusal for an unlabelled step falls back to its
// op, so an author can locate a step that carries no id.
func TestUnknownOpNamesTheStepByOp(t *testing.T) {
	err := Flow{Steps: []Step{{Op: Op("exfiltrate")}}}.Validate()
	if err == nil || !strings.Contains(err.Error(), `step "exfiltrate"`) {
		t.Fatalf("expected the refusal to name the step by its op, got %v", err)
	}
}

// TestCompileRefusesMalformedActions is the per-action admission table: every action that
// is missing a required field or carries an unparseable expression, template, or body is
// refused at compile, so no such flow ever reaches the interpreter.
func TestCompileRefusesMalformedActions(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"http with no url", `{"steps":[{"op":"http","http":{}}]}`, "needs a url"},
		{"http with a bad url template", `{"steps":[{"op":"http","http":{"url":"https://x/{{"}}]}`, "unclosed"},
		{"http with a bad method template", `{"steps":[{"op":"http","http":{"url":"u","method":"{{1 +}}"}}]}`, "flow:"},
		{"http with a bad header template", `{"steps":[{"op":"http","http":{"url":"u","headers":{"a":"{{"}}}]}`, "unclosed"},
		{"http with a bad query template", `{"steps":[{"op":"http","http":{"url":"u","query":{"a":"{{"}}}]}`, "unclosed"},
		{"http with a bad body template", `{"steps":[{"op":"http","http":{"url":"u","body":{"a":"{{"}}}]}`, "unclosed"},
		{"http with a bad body list leaf", `{"steps":[{"op":"http","http":{"url":"u","body":["{{"]}}]}`, "unclosed"},
		{"transform with no mode", `{"steps":[{"op":"transform","transform":{"source":"config"}}]}`, "exactly one of"},
		{"transform with two modes", `{"steps":[{"op":"transform","transform":{"value":"1","map":"it"}}]}`, "exactly one of"},
		{"transform with a select and a value", `{"steps":[{"op":"transform","transform":{"value":"1","select":{"a":"it"}}}]}`, "exactly one of"},
		{"transform with a bad source", `{"steps":[{"op":"transform","transform":{"source":"1 +","value":"it"}}]}`, "flow:"},
		{"transform with a bad value", `{"steps":[{"op":"transform","transform":{"value":"1 +"}}]}`, "flow:"},
		{"transform with a bad filter", `{"steps":[{"op":"transform","transform":{"source":"config.l","filter":"1 +"}}]}`, "flow:"},
		{"transform with a bad map", `{"steps":[{"op":"transform","transform":{"source":"config.l","map":"1 +"}}]}`, "flow:"},
		{"transform with a bad select", `{"steps":[{"op":"transform","transform":{"select":{"a":"1 +"}}}]}`, "flow:"},
		{"condition with a bad predicate", `{"steps":[{"op":"condition","condition":{"if":"1 +"}}]}`, "flow:"},
		{"condition with a bad else branch", `{"steps":[{"op":"condition","condition":{"if":"true","else":[{"op":"exec","exec":{}}]}}]}`, "needs a command"},
		{"loop with neither over nor count", `{"steps":[{"op":"loop","loop":{}}]}`, "needs over or a positive count"},
		{"loop with a negative count", `{"steps":[{"op":"loop","loop":{"count":-1}}]}`, "needs over or a positive count"},
		{"loop with a bad over", `{"steps":[{"op":"loop","loop":{"over":"1 +"}}]}`, "flow:"},
		{"loop with a bad collect", `{"steps":[{"op":"loop","loop":{"count":1,"collect":"1 +"}}]}`, "flow:"},
		{"call with no tool", `{"steps":[{"op":"call","call":{}}]}`, "needs a tool name"},
		{"call with a bad tool template", `{"steps":[{"op":"call","call":{"tool":"{{"}}]}`, "unclosed"},
		{"call with a bad input template", `{"steps":[{"op":"call","call":{"tool":"t","input":{"a":"{{"}}}]}`, "unclosed"},
		{"return with a bad body", `{"steps":[{"op":"return","return":{"value":"{{"}}]}`, "unclosed"},
		{"assert with no condition", `{"steps":[{"op":"assert","assert":{}}]}`, "needs a condition"},
		{"assert with a bad condition", `{"steps":[{"op":"assert","assert":{"that":"1 +"}}]}`, "flow:"},
		{"assert with a bad message", `{"steps":[{"op":"assert","assert":{"that":"true","message":"{{"}}]}`, "unclosed"},
		{"exec with no command", `{"steps":[{"op":"exec","exec":{}}]}`, "needs a command"},
		{"exec with a bad command template", `{"steps":[{"op":"exec","exec":{"command":"{{"}}]}`, "unclosed"},
		{"dependency with no name", `{"steps":[{"op":"dependency","dependency":{}}]}`, "needs a name"},
		{"dependency with a bad name template", `{"steps":[{"op":"dependency","dependency":{"name":"{{"}}]}`, "unclosed"},
		{"confirm with no message", `{"steps":[{"op":"confirm","confirm":{}}]}`, "needs a message"},
		{"confirm with a bad message template", `{"steps":[{"op":"confirm","confirm":{"message":"{{"}}]}`, "unclosed"},
		{"secret with no ref", `{"steps":[{"op":"secret","secret":{"sink":"s"}}]}`, "needs a ref"},
		{"secret with no sink", `{"steps":[{"op":"secret","secret":{"ref":"r"}}]}`, "needs a sink"},
		{"secret with a bad ref template", `{"steps":[{"op":"secret","secret":{"ref":"{{","sink":"s"}}]}`, "unclosed"},
		{"secret with a bad sink template", `{"steps":[{"op":"secret","secret":{"ref":"r","sink":"{{"}}]}`, "unclosed"},
		{"secret with a bad target template", `{"steps":[{"op":"secret","secret":{"ref":"r","sink":"s","target":{"a":"{{"}}}]}`, "unclosed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode([]byte(c.src))
			if err == nil {
				t.Fatal("expected the flow to be refused at compile")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected %q, got %v", c.want, err)
			}
			assertTerminal(t, err)
		})
	}
}

// TestDecodeRefusesUnusableInput proves a flow that is not even JSON never becomes a Flow.
func TestDecodeRefusesUnusableInput(t *testing.T) {
	for _, raw := range []string{"", "{", `{"steps":"nope"}`} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("expected %q to be refused", raw)
		}
	}
}

// TestPortsFailClosed proves every step with an external effect refuses to run when its
// port is not wired, rather than degrading to a no-op that a flow would read as success.
func TestPortsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"http", `{"steps":[{"op":"http","http":{"url":"https://x"}}]}`, "no transport is configured"},
		{"call", `{"steps":[{"op":"call","call":{"tool":"t"}}]}`, "no tool caller is configured"},
		{"exec", `{"steps":[{"op":"exec","exec":{"command":"c"}}]}`, "no runner is configured"},
		{"dependency", `{"steps":[{"op":"dependency","dependency":{"name":"n"}}]}`, "no resolver is configured"},
		{"confirm", `{"steps":[{"op":"confirm","confirm":{"message":"m"}}]}`, "no prompter is configured"},
		{"secret", `{"steps":[{"op":"secret","secret":{"ref":"r","sink":"s"}}]}`, "no credential sink is configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runFlow(t, c.src, nil)
			if err == nil {
				t.Fatal("expected the step to fail closed with no port wired")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected %q, got %v", c.want, err)
			}
			assertTerminal(t, err)
		})
	}
}

// TestTemplateRenderErrorsStopTheStep proves an expression that cannot be evaluated at run
// time (an unbound reference in a rendered field) fails the step everywhere a field is
// templated, rather than rendering an empty value into a request, a command, or a secret
// target.
func TestTemplateRenderErrorsStopTheStep(t *testing.T) {
	cases := []struct {
		name string
		src  string
		opts []Option
	}{
		{"http method", `{"steps":[{"op":"http","http":{"url":"https://x","method":"{{nope}}"}}]}`, []Option{WithHTTP(&fakeHTTP{})}},
		{"http url", `{"steps":[{"op":"http","http":{"url":"https://x/{{nope}}"}}]}`, []Option{WithHTTP(&fakeHTTP{})}},
		{"http header", `{"steps":[{"op":"http","http":{"url":"https://x","headers":{"a":"{{nope}}"}}}]}`, []Option{WithHTTP(&fakeHTTP{})}},
		{"http query", `{"steps":[{"op":"http","http":{"url":"https://x","query":{"a":"{{nope}}"}}}]}`, []Option{WithHTTP(&fakeHTTP{})}},
		{"http body", `{"steps":[{"op":"http","http":{"url":"https://x","body":{"a":"{{nope}}"}}}]}`, []Option{WithHTTP(&fakeHTTP{})}},
		{"call tool", `{"steps":[{"op":"call","call":{"tool":"{{nope}}"}}]}`, []Option{WithTools(&fakeTools{})}},
		{"call input", `{"steps":[{"op":"call","call":{"tool":"t","input":{"a":"{{nope}}"}}}]}`, []Option{WithTools(&fakeTools{})}},
		{"exec command", `{"steps":[{"op":"exec","exec":{"command":"run {{nope}}"}}]}`, []Option{WithExec(&fakeExecer{})}},
		{"dependency name", `{"steps":[{"op":"dependency","dependency":{"name":"{{nope}}"}}]}`, []Option{WithDependencies(&fakeResolver{})}},
		{"confirm message", `{"steps":[{"op":"confirm","confirm":{"message":"{{nope}}"}}]}`, []Option{WithConfirm(&fakeConfirmer{})}},
		{"secret ref", `{"steps":[{"op":"secret","secret":{"ref":"{{nope}}","sink":"s"}}]}`, []Option{WithCredentialSink(&fakeSink{})}},
		{"secret sink", `{"steps":[{"op":"secret","secret":{"ref":"r","sink":"{{nope}}"}}]}`, []Option{WithCredentialSink(&fakeSink{})}},
		{"secret target", `{"steps":[{"op":"secret","secret":{"ref":"r","sink":"s","target":{"a":"{{nope}}"}}}]}`, []Option{WithCredentialSink(&fakeSink{})}},
		{"return value", `{"steps":[{"op":"return","return":{"value":"{{nope}}"}}]}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runFlow(t, c.src, nil, c.opts...)
			if err == nil {
				t.Fatal("expected the unbound reference to stop the step")
			}
			if !strings.Contains(err.Error(), "unknown reference") {
				t.Fatalf("expected an unknown-reference error, got %v", err)
			}
			assertTerminal(t, err)
		})
	}
}

// TestHTTPStepExposesTheWholeResponse proves the step output carries the status, the
// headers, and the decoded body, so a later step can branch on any of them.
func TestHTTPStepExposesTheWholeResponse(t *testing.T) {
	http := &fakeHTTP{resp: HTTPResponse{
		Status:  201,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    map[string]any{"id": "x1"},
		Raw:     []byte(`{"id":"x1"}`),
	}}
	out, err := runFlow(t, `{"steps":[
		{"id":"r","op":"http","http":{"method":"{{config.method}}","url":"https://x/{{config.id}}","headers":{"X-Tok":"t"},"query":{"q":"{{config.id}}"},"body":{"n":"{{config.id}}"}}},
		{"op":"return","return":{"value":{"status":"{{steps.r.status}}","ct":"{{steps.r.headers['Content-Type']}}","id":"{{steps.r.body.id}}"}}}
	]}`, map[string]any{"method": "POST", "id": "x1"}, WithHTTP(http))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("want an object result, got %T", out)
	}
	if got["status"] != float64(201) || got["ct"] != "application/json" || got["id"] != "x1" {
		t.Fatalf("response not fully exposed: %#v", got)
	}
	if len(http.gotReqs) != 1 {
		t.Fatalf("want one request, got %d", len(http.gotReqs))
	}
	req := http.gotReqs[0]
	if req.Method != "POST" || req.URL != "https://x/x1" {
		t.Fatalf("request not templated: %+v", req)
	}
	if req.Headers["X-Tok"] != "t" || req.Query["q"] != "x1" {
		t.Fatalf("headers/query not rendered: %+v", req)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("body not valid json: %v", err)
	}
	if body["n"] != "x1" {
		t.Fatalf("body not templated: %#v", body)
	}
}

// TestHTTPTransportErrorSurfaces proves the host transport's error stops the flow rather
// than being folded into an empty response a later step would read as success.
func TestHTTPTransportErrorSurfaces(t *testing.T) {
	boom := errors.New("dial refused")
	_, err := runFlow(t, `{"steps":[{"op":"http","http":{"url":"https://x"}}]}`, nil,
		WithHTTP(&fakeHTTP{err: boom}))
	if !errors.Is(err, boom) {
		t.Fatalf("expected the transport error to surface, got %v", err)
	}
}

// TestHTTPPayloadCapRefusesAnOversizeResponse proves a response larger than the cap fails
// the step instead of being exposed to later steps, so a hostile endpoint cannot amplify.
func TestHTTPPayloadCapRefusesAnOversizeResponse(t *testing.T) {
	http := &fakeHTTP{resp: HTTPResponse{Status: 200, Body: "big", Raw: make([]byte, 64)}}
	_, err := runFlow(t, `{"limits":{"maxPayloadBytes":32},"steps":[
		{"id":"r","op":"http","http":{"url":"https://x"}},
		{"op":"return","return":{"value":"{{steps.r.body}}"}}
	]}`, nil, WithHTTP(http))
	if err == nil || !strings.Contains(err.Error(), "payload cap") {
		t.Fatalf("expected the payload cap to refuse the response, got %v", err)
	}
	assertTerminal(t, err)
}

// TestHTTPPayloadUnderCapPasses proves the cap is a ceiling, not a blanket refusal.
func TestHTTPPayloadUnderCapPasses(t *testing.T) {
	http := &fakeHTTP{resp: HTTPResponse{Status: 200, Body: "small", Raw: make([]byte, 8)}}
	out, err := runFlow(t, `{"limits":{"maxPayloadBytes":32},"steps":[
		{"id":"r","op":"http","http":{"url":"https://x"}},
		{"op":"return","return":{"value":"{{steps.r.body}}"}}
	]}`, nil, WithHTTP(http))
	if err != nil || out != "small" {
		t.Fatalf("want the body through, got %v err=%v", out, err)
	}
}

// TestTransformRefusesAListModeOnANonList proves filter and map refuse a source that is not
// a list rather than silently yielding an empty result a later step would read as "none
// matched".
func TestTransformRefusesAListModeOnANonList(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"filter", `{"steps":[{"op":"transform","transform":{"source":"config.n","filter":"it"}}]}`, "filter source must be a list"},
		{"map", `{"steps":[{"op":"transform","transform":{"source":"config.n","map":"it"}}]}`, "map source must be a list"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runFlow(t, c.src, map[string]any{"n": float64(1)})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected %q, got %v", c.want, err)
			}
			assertTerminal(t, err)
		})
	}
}

// TestTransformElementErrorsStopTheStep proves an expression that fails on one element
// fails the whole transform, so a partial result is never recorded as the step's output.
func TestTransformElementErrorsStopTheStep(t *testing.T) {
	cfg := map[string]any{"l": []any{map[string]any{"a": float64(1)}, "not-an-object"}}
	for _, src := range []string{
		`{"steps":[{"op":"transform","transform":{"source":"config.l","filter":"it.a"}}]}`,
		`{"steps":[{"op":"transform","transform":{"source":"config.l","map":"it.a"}}]}`,
		`{"steps":[{"op":"transform","transform":{"source":"config.l","select":{"a":"it.nope.deep"}}}]}`,
	} {
		if _, err := runFlow(t, src, cfg); err == nil {
			t.Fatalf("expected the element error to stop the transform: %s", src)
		}
	}
}

// TestTransformSelectProjects proves the select mode builds an object from the source bound
// as "it".
func TestTransformSelectProjects(t *testing.T) {
	out, err := runFlow(t, `{"steps":[
		{"id":"p","op":"transform","transform":{"source":"config.u","select":{"name":"it.first + ' ' + it.last","upper":"upper(it.first)"}}},
		{"op":"return","return":{"value":"{{steps.p}}"}}
	]}`, map[string]any{"u": map[string]any{"first": "ada", "last": "l"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok || got["name"] != "ada l" || got["upper"] != "ADA" {
		t.Fatalf("select did not project: %#v", out)
	}
}

// TestLoopRefusesANonListSource proves a loop whose "over" does not yield a list fails
// rather than iterating zero times and looking like an empty result.
func TestLoopRefusesANonListSource(t *testing.T) {
	_, err := runFlow(t, `{"steps":[{"op":"loop","loop":{"over":"config.n","body":[]}}]}`,
		map[string]any{"n": float64(3)})
	if err == nil || !strings.Contains(err.Error(), "loop over must yield a list") {
		t.Fatalf("expected a non-list loop source to be refused, got %v", err)
	}
	assertTerminal(t, err)
}

// TestLoopCollectErrorStopsTheLoop proves a collect expression that fails on an iteration
// fails the flow rather than collecting a hole.
func TestLoopCollectErrorStopsTheLoop(t *testing.T) {
	_, err := runFlow(t, `{"steps":[{"op":"loop","loop":{"count":2,"collect":"item.nope"}}]}`, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot index into") {
		t.Fatalf("expected the collect error to stop the loop, got %v", err)
	}
}

// TestReturnInsideALoopStopsCollecting proves a return that fires mid-iteration ends the
// flow and does not collect the partially executed iteration.
func TestReturnInsideALoopStopsCollecting(t *testing.T) {
	out, err := runFlow(t, `{"steps":[
		{"id":"l","op":"loop","loop":{"count":5,"collect":"index","body":[
			{"op":"condition","condition":{"if":"index == 2","then":[
				{"op":"return","return":{"value":"{{index}}"}}
			]}}
		]}}
	]}`, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != float64(2) {
		t.Fatalf("want the return value from inside the loop, got %v", out)
	}
}

// TestReturnWithNoValueYieldsNull proves a bare return stops the flow with a null result
// rather than falling through to the following steps.
func TestReturnWithNoValueYieldsNull(t *testing.T) {
	ex := &fakeExecer{}
	out, err := runFlow(t, `{"steps":[
		{"op":"return","return":{}},
		{"op":"exec","exec":{"command":"must-not-run"}}
	]}`, nil, WithExec(ex))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != nil {
		t.Fatalf("want a null result, got %v", out)
	}
	if ex.lastCmd != "" {
		t.Fatalf("a step after the return ran: %q", ex.lastCmd)
	}
}

// TestExecAllowNonzeroExposesTheExitCode proves a step that opts in to a nonzero exit keeps
// running and hands the exit code to later steps.
func TestExecAllowNonzeroExposesTheExitCode(t *testing.T) {
	ex := &fakeExecer{res: ExecResult{ExitCode: 3, Output: "missing"}}
	out, err := runFlow(t, `{"steps":[
		{"id":"r","op":"exec","exec":{"command":"probe","allowNonzero":true}},
		{"op":"return","return":{"value":"{{steps.r.exitCode}}"}}
	]}`, nil, WithExec(ex))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != float64(3) {
		t.Fatalf("want the exit code exposed, got %v", out)
	}
}

// TestExecFailureFaultTruncatesOutput proves a chatty failing command cannot produce an
// unbounded error message, and that the tail (where the reason usually is) is what survives.
func TestExecFailureFaultTruncatesOutput(t *testing.T) {
	tail := "the real reason"
	long := strings.Repeat("x", maxFaultOutputBytes*2) + tail
	err := execFailedFault("noisy", ExecResult{ExitCode: 2, Output: long})
	msg := err.Error()
	if len(msg) > maxFaultOutputBytes+200 {
		t.Fatalf("fault message is unbounded: %d bytes", len(msg))
	}
	if !strings.Contains(msg, tail) {
		t.Fatal("the tail of the output should survive truncation")
	}
	if !strings.Contains(msg, "...") {
		t.Fatal("a truncated output should be marked as truncated")
	}
}

// TestPortErrorsSurface proves an error from a host port stops the flow, is reported to the
// observer, and is not swallowed into a successful step.
func TestPortErrorsSurface(t *testing.T) {
	boom := errors.New("port refused")
	cases := []struct {
		name string
		src  string
		opts []Option
	}{
		{"exec", `{"steps":[{"op":"exec","exec":{"command":"c"}}]}`, []Option{WithExec(&fakeExecer{err: boom})}},
		{"dependency", `{"steps":[{"op":"dependency","dependency":{"name":"n"}}]}`, []Option{WithDependencies(&fakeResolver{err: boom})}},
		{"confirm", `{"steps":[{"op":"confirm","confirm":{"message":"m"}}]}`, []Option{WithConfirm(&fakeConfirmer{err: boom})}},
		{"secret", `{"steps":[{"op":"secret","secret":{"ref":"r","sink":"s"}}]}`, []Option{WithCredentialSink(&fakeSink{err: boom})}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := &recordingObserver{}
			_, err := runFlow(t, c.src, nil, append(c.opts, WithObserver(obs))...)
			if !errors.Is(err, boom) {
				t.Fatalf("expected the port error to surface, got %v", err)
			}
			if c.name == "confirm" {
				return // a confirm step is not an observable effect
			}
			last := obs.events[len(obs.events)-1]
			if last.Phase != StepEnd || !errors.Is(last.Err, boom) {
				t.Fatalf("the failure should be reported to the observer, got %+v", last)
			}
		})
	}
}

// TestSecretStepReportsOnlyTheReference proves the observer sees the sink and the reference
// and never a value, which is the whole point of the secret step: the flow declares which
// secret goes where, and the value stays inside the sink.
func TestSecretStepReportsOnlyTheReference(t *testing.T) {
	sink := &fakeSink{}
	obs := &recordingObserver{}
	_, err := runFlow(t, `{"steps":[
		{"op":"secret","secret":{"ref":"{{config.ref}}","sink":"fly","target":{"app":"{{config.app}}","key":"API_KEY"}}}
	]}`, map[string]any{"ref": "vault:token", "app": "shop"},
		WithCredentialSink(sink), WithObserver(obs))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.gotSink != "fly" || sink.gotRef != "vault:token" {
		t.Fatalf("the sink got the wrong reference: %+v", sink)
	}
	if sink.gotTarget["app"] != "shop" || sink.gotTarget["key"] != "API_KEY" {
		t.Fatalf("the target was not rendered: %+v", sink.gotTarget)
	}
	for _, ev := range obs.events {
		if ev.Detail != "fly:vault:token" {
			t.Fatalf("a secret event should carry only the sink and reference, got %q", ev.Detail)
		}
		if ev.Output != "" {
			t.Fatalf("a secret event must never carry output, got %q", ev.Output)
		}
	}
}

// TestRunCompilesADirectlyConstructedFlow proves a Flow built in Go (never Decoded) is
// compiled by Run, so it is admitted by the same gate: a well-formed one runs and a
// malformed one is refused at Run rather than executing partially.
func TestRunCompilesADirectlyConstructedFlow(t *testing.T) {
	good := Flow{Steps: []Step{
		{Op: OpReturn, Return: &ReturnAction{Value: json.RawMessage(`"ok"`)}},
	}}
	out, err := New().Run(context.Background(), good, nil)
	if err != nil || out != "ok" {
		t.Fatalf("want ok, got %v err=%v", out, err)
	}

	bad := Flow{Steps: []Step{
		{Op: OpExec, Exec: &ExecAction{}},
		{Op: OpReturn, Return: &ReturnAction{Value: json.RawMessage(`"never"`)}},
	}}
	ex := &fakeExecer{}
	if _, err := New(WithExec(ex)).Run(context.Background(), bad, nil); err == nil {
		t.Fatal("expected a malformed direct flow to be refused at Run")
	}
	if ex.lastCmd != "" {
		t.Fatal("a refused flow must not have executed a step")
	}
}

// assertTerminal proves a refusal is classified Terminal, so a caller retries a transient
// failure but never retries a flow into the same wall.
func assertTerminal(t *testing.T, err error) {
	t.Helper()
	var fe *fault.Error
	if !errors.As(err, &fe) {
		t.Fatalf("expected a fault, got %T: %v", err, err)
	}
	if fe.Class != fault.Terminal {
		t.Fatalf("expected a Terminal fault, got %v", fe.Class)
	}
}
