package flow

import (
	"encoding/json"

	"github.com/ionalpha/flynn/fault"
)

// Op names one action an interpreter can execute. The set is closed and small: it
// is the entire vocabulary a declarative flow has, chosen so a runtime-authored
// spec can express useful behaviour without any path to arbitrary code.
type Op string

const (
	// OpHTTP performs one HTTP request through the injected transport.
	OpHTTP Op = "http"
	// OpTransform reshapes a value (extract, project, filter, map) using expressions.
	OpTransform Op = "transform"
	// OpCondition branches on a boolean expression.
	OpCondition Op = "condition"
	// OpLoop repeats a body over a list or a fixed count.
	OpLoop Op = "loop"
	// OpCall invokes another tool or extension through the injected caller.
	OpCall Op = "call"
	// OpReturn yields the flow's result and stops.
	OpReturn Op = "return"
	// OpAssert checks a condition and fails the flow when it is false, so a flow can
	// verify an outcome (a resource exists, a status is healthy) before continuing.
	OpAssert Op = "assert"
	// OpExec runs a command through the injected runner, which confines it in the
	// sandbox, so a flow can drive an external command-line program.
	OpExec Op = "exec"
	// OpDependency ensures an external program is present, provisioning a pinned build
	// when it is missing, and yields the path to run it.
	OpDependency Op = "dependency"
	// OpConfirm pauses the flow to show the user a message and wait for them to approve
	// before continuing, so an interactive step (a browser login, a destructive action)
	// is surfaced and consented to rather than happening silently.
	OpConfirm Op = "confirm"
	// OpSecret materializes a secret into a target's secret store (a provider's secrets,
	// a deployed workload's environment) by reference. The step names only the reference,
	// the sink, and the target; the host resolves the reference to its value and delivers
	// it, so the secret value never enters the flow as data, a rendered command, or a log.
	OpSecret Op = "secret"
)

// Flow is a declarative procedure: an ordered list of steps the interpreter runs to
// produce a result. It decodes from the JSON an extension surface carries, so the
// stored spec is exactly what executes.
type Flow struct {
	Steps  []Step  `json:"steps"`
	Limits *Limits `json:"limits,omitempty"`

	// compiled is the parsed executable form, built once by Decode. It is read-only
	// after compile, so the same Flow value can run concurrently. A Flow constructed
	// directly (without Decode) is compiled by Run instead.
	compiled *compiledFlow
}

// Step is one action in a flow. ID names the step so later steps can reference its
// output by path (steps.<id>). Exactly one action field is set, and it must match
// Op; Validate enforces that pairing so a malformed step is rejected before it runs.
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

// HTTPAction is a single request. Method, URL, and the values of Headers and Query
// are templated; Body is deep-templated (every string leaf), so a request is built
// from prior outputs and config without code.
type HTTPAction struct {
	Method  string            `json:"method,omitempty"` // default GET
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// TransformAction reshapes Source (an expression yielding the input value) into a
// new value. Exactly one mode applies:
//   - Value: yield the expression's result directly (extract / rename).
//   - Select: build an object from outKey -> expression, each evaluated against the
//     source bound as "it".
//   - Filter (lists): keep elements where the expression is truthy, element as "it".
//   - Map (lists): replace each element with the expression's result, element as "it".
type TransformAction struct {
	Source string            `json:"source,omitempty"`
	Value  string            `json:"value,omitempty"`
	Select map[string]string `json:"select,omitempty"`
	Filter string            `json:"filter,omitempty"`
	Map    string            `json:"map,omitempty"`
}

// ConditionAction branches: if If is truthy, run Then, else run Else. A branch is
// itself a list of steps, so conditions nest.
type ConditionAction struct {
	If   string `json:"if"`
	Then []Step `json:"then,omitempty"`
	Else []Step `json:"else,omitempty"`
}

// LoopAction repeats Body. Over is an expression yielding a list to iterate; when
// empty, Count gives a fixed iteration count. As names the element binding (default
// "item"); the zero-based index is always bound as "index". When Collect is set, it
// is evaluated each iteration and the results are gathered into this step's output
// list, so a loop can build a result.
type LoopAction struct {
	Over    string `json:"over,omitempty"`
	Count   int    `json:"count,omitempty"`
	As      string `json:"as,omitempty"`
	Body    []Step `json:"body,omitempty"`
	Collect string `json:"collect,omitempty"`
}

// CallAction invokes another tool or extension by name (templated) with an Input
// object (deep-templated). The result becomes this step's output.
type CallAction struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input,omitempty"`
}

// ReturnAction yields the flow result. Value is deep-templated, so it can be a
// literal shape with holes or a single expression yielding a typed value.
type ReturnAction struct {
	Value json.RawMessage `json:"value,omitempty"`
}

// AssertAction fails the flow when That is not truthy, with an optional templated
// Message. It is how a flow verifies an outcome and stops with a clear error when the
// outcome does not hold, rather than continuing on a false assumption.
type AssertAction struct {
	That    string `json:"that"`
	Message string `json:"message,omitempty"`
}

// ExecAction runs Command (templated) through the injected runner, which confines it in
// the sandbox. The step output is {exitCode, output}. A nonzero exit fails the step
// unless AllowNonzero is set, so a command failure stops the flow by default; set
// AllowNonzero to inspect the exit code in a later step instead.
type ExecAction struct {
	Command      string `json:"command"`
	AllowNonzero bool   `json:"allowNonzero,omitempty"`
}

// DependencyAction ensures the named external program is present, provisioning a pinned
// build when it is missing, and yields {path} to run it. Name is templated.
type DependencyAction struct {
	Name string `json:"name"`
}

// ConfirmAction pauses the flow to show Message (templated) and wait for the user to
// approve before continuing. It is how a flow makes an interactive step visible and gets
// the operator's go-ahead rather than acting silently, and how a non-interactive run stops
// with a clear instruction instead of blocking. It produces no output.
type ConfirmAction struct {
	Message string `json:"message"`
}

// SecretAction materializes the secret named by Ref into Sink's secret store, with Target
// carrying the sink's parameters (for a hosting provider: the app and the key name). Ref,
// Sink, and every Target value are templated, so the reference and target are built from
// config and prior steps. The host resolves Ref to its value and hands it to the sink, so
// the value never appears in the flow's expressions, the rendered command, or a log: a
// step declares WHICH secret goes WHERE, never the secret itself. It produces no output.
type SecretAction struct {
	Ref    string            `json:"ref"`
	Sink   string            `json:"sink"`
	Target map[string]string `json:"target,omitempty"`
}

// Decode parses a Flow from JSON, validates it, and compiles it: every expression,
// template, and templated body is parsed exactly once here, and execution reuses
// the parsed form. A flow that does not compile is never returned, so the
// interpreter only ever runs well-formed flows.
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

// Validate checks a flow's structure: every step's Op matches exactly one action
// block, ids are unique, control-flow bodies recurse, and every expression and
// template parses. It is static admission: a flow that passes never fails later for
// a structural reason, only for a runtime value error or a resource cap.
func (f Flow) Validate() error {
	_, err := compileFlow(f)
	return err
}

// actionBlocks counts how many action blocks a step carries (regardless of Op) and
// reports whether the block matching Op is among them. A step is well-formed only
// when it carries exactly one block and that block matches its Op, so both a stray
// extra block and an op/block mismatch are caught.
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
