package flow

import (
	"encoding/json"

	"github.com/ionalpha/flynn/fault"
)

// Op names one action. The set is closed, so a runtime-authored spec has no path to
// arbitrary code.
type Op string

const (
	// OpHTTP performs one HTTP request through the injected transport.
	OpHTTP Op = "http"
	// OpTransform reshapes a value (extract, project, filter, map).
	OpTransform Op = "transform"
	// OpCondition branches on a boolean expression.
	OpCondition Op = "condition"
	// OpLoop repeats a body over a list or a fixed count.
	OpLoop Op = "loop"
	// OpCall invokes another tool or extension through the injected caller.
	OpCall Op = "call"
	// OpReturn yields the flow's result and stops.
	OpReturn Op = "return"
	// OpAssert fails the flow when a condition is false, so a flow can verify an
	// outcome before continuing.
	OpAssert Op = "assert"
	// OpExec runs a command through the injected runner, confined in the sandbox.
	OpExec Op = "exec"
	// OpDependency ensures an external program is present, provisioning a pinned
	// build if missing, and yields its path.
	OpDependency Op = "dependency"
	// OpConfirm shows the user a message and waits for approval, so an interactive
	// or destructive step is consented to.
	OpConfirm Op = "confirm"
	// OpSecret materializes a secret by reference into a target's secret store. The
	// host resolves and delivers the value, so it never enters the flow, a command
	// or a log.
	OpSecret Op = "secret"
)

// Flow is a declarative procedure, an ordered list of steps. It decodes from the
// JSON an extension carries, so the stored spec is what executes.
type Flow struct {
	Steps  []Step  `json:"steps"`
	Limits *Limits `json:"limits,omitempty"`

	// compiled is the read-only executable form built by Decode. A Flow built
	// without Decode is compiled by Run.
	compiled *compiledFlow
}

// Step is one action. Later steps read its output at steps.<id>. Exactly one action
// field is set and it must match Op, which Validate checks before it runs.
type Step struct {
	ID  string `json:"id,omitempty"`
	Op  Op     `json:"op"`
	Doc string `json:"doc,omitempty"`

	HTTP       *HTTPAction       `json:"http,omitempty"`
	Transform  *TransformAction  `json:"transform,omitempty"`
	Condition  *ConditionAction  `json:"condition,omitempty"`
	Loop       *LoopAction       `json:"loop,omitempty"`
	Call       *CallAction       `json:"call,omitempty"`
	Return     *ReturnAction     `json:"return,omitempty"`
	Assert     *AssertAction     `json:"assert,omitempty"`
	Exec       *ExecAction       `json:"exec,omitempty"`
	Dependency *DependencyAction `json:"dependency,omitempty"`
	Confirm    *ConfirmAction    `json:"confirm,omitempty"`
	Secret     *SecretAction     `json:"secret,omitempty"`
}

// HTTPAction is one request. Method, URL and the Headers and Query values are
// templated; Body is deep-templated (every string leaf).
type HTTPAction struct {
	Method  string            `json:"method,omitempty"` // default GET
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// TransformAction reshapes Source (an expression) with exactly one mode:
//   - Value: yield the expression's result (extract, rename).
//   - Select: build an object of outKey to expression, with the source as "it".
//   - Filter (lists): keep elements where the expression is truthy, element as "it".
//   - Map (lists): replace each element with the expression's result, element as "it".
type TransformAction struct {
	Source string            `json:"source,omitempty"`
	Value  string            `json:"value,omitempty"`
	Select map[string]string `json:"select,omitempty"`
	Filter string            `json:"filter,omitempty"`
	Map    string            `json:"map,omitempty"`
}

// ConditionAction runs Then if If is truthy, else Else. Branches are step lists, so
// conditions nest.
type ConditionAction struct {
	If   string `json:"if"`
	Then []Step `json:"then,omitempty"`
	Else []Step `json:"else,omitempty"`
}

// LoopAction repeats Body over the list Over yields, or Count times when Over is
// empty. As names the element (default "item") and "index" is the zero-based
// index. Collect, when set, is evaluated each iteration into the step's output list.
type LoopAction struct {
	Over    string `json:"over,omitempty"`
	Count   int    `json:"count,omitempty"`
	As      string `json:"as,omitempty"`
	Body    []Step `json:"body,omitempty"`
	Collect string `json:"collect,omitempty"`
}

// CallAction invokes a tool or extension by templated name with a deep-templated
// Input; the result is the step's output.
type CallAction struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input,omitempty"`
}

// ReturnAction yields the flow result. Value is deep-templated: a literal with holes
// or one expression yielding a typed value.
type ReturnAction struct {
	Value json.RawMessage `json:"value,omitempty"`
}

// AssertAction fails the flow with optional templated Message when That is not
// truthy.
type AssertAction struct {
	That    string `json:"that"`
	Message string `json:"message,omitempty"`
}

// ExecAction runs templated Command through the sandbox runner; the output is
// {exitCode, output}. A nonzero exit fails the step unless AllowNonzero is set, for
// a later step to inspect the code.
type ExecAction struct {
	Command      string `json:"command"`
	AllowNonzero bool   `json:"allowNonzero,omitempty"`
}

// DependencyAction ensures the program Name (templated) is present, provisioning a
// pinned build if missing, and yields {path}.
type DependencyAction struct {
	Name string `json:"name"`
}

// ConfirmAction shows templated Message and waits for approval. A non-interactive
// run stops with a clear instruction instead of blocking. It produces no output.
type ConfirmAction struct {
	Message string `json:"message"`
}

// SecretAction materializes the secret Ref into Sink's secret store; Target holds
// the sink's parameters (for a hosting provider, the app and key name). All are
// templated. The host resolves Ref and hands the value to the sink, so a step names
// which secret goes where and the value never appears in expressions, commands or
// logs. It produces no output.
type SecretAction struct {
	Ref    string            `json:"ref"`
	Sink   string            `json:"sink"`
	Target map[string]string `json:"target,omitempty"`
}

// Decode parses, validates and compiles a Flow from JSON, parsing every expression
// and template once. A flow that does not compile is never returned.
func Decode(raw []byte) (Flow, error) {
	var f Flow
	if len(raw) == 0 {
		return Flow{}, fault.New(fault.Terminal, "flow_empty", "flow: empty definition")
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return Flow{}, fault.Wrap(fault.Terminal, "flow_decode", err)
	}
	cf, err := compileFlow(f)
	if err != nil {
		return Flow{}, err
	}
	f.compiled = cf
	return f, nil
}

// Validate checks structure: each step's Op matches exactly one action block, ids
// are unique, bodies recurse, and every expression and template parses. A flow that
// passes can fail later only on a runtime value or a resource cap.
func (f Flow) Validate() error {
	_, err := compileFlow(f)
	return err
}

// actionBlocks counts a step's action blocks and reports whether Op's block is among
// them, so both a stray extra block and an op mismatch are caught.
func actionBlocks(s Step) (total int, matchesOp bool) {
	for _, b := range []struct {
		present bool
		op      Op
	}{
		{s.HTTP != nil, OpHTTP},
		{s.Transform != nil, OpTransform},
		{s.Condition != nil, OpCondition},
		{s.Loop != nil, OpLoop},
		{s.Call != nil, OpCall},
		{s.Return != nil, OpReturn},
		{s.Assert != nil, OpAssert},
		{s.Exec != nil, OpExec},
		{s.Dependency != nil, OpDependency},
		{s.Confirm != nil, OpConfirm},
		{s.Secret != nil, OpSecret},
	} {
		if b.present {
			total++
			if b.op == s.Op {
				matchesOp = true
			}
		}
	}
	return total, matchesOp
}

func stepLabel(s Step) string {
	if s.ID != "" {
		return s.ID
	}
	return string(s.Op)
}
