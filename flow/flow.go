package flow

import (
	"encoding/json"
	"fmt"

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
)

// Flow is a declarative procedure: an ordered list of steps the interpreter runs to
// produce a result. It decodes from the JSON an extension surface carries, so the
// stored spec is exactly what executes.
type Flow struct {
	Steps  []Step  `json:"steps"`
	Limits *Limits `json:"limits,omitempty"`
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

// Decode parses a Flow from JSON and validates it. A flow that does not validate is
// never returned, so the interpreter only ever runs well-formed flows.
func Decode(raw []byte) (Flow, error) {
	var f Flow
	if len(raw) == 0 {
		return Flow{}, fault.New(fault.Terminal, "flow_empty", "flow: empty definition")
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return Flow{}, fault.Wrap(fault.Terminal, "flow_decode", err)
	}
	if err := f.Validate(); err != nil {
		return Flow{}, err
	}
	return f, nil
}

// Validate checks a flow's structure: every step's Op matches exactly one action
// block, ids are unique, control-flow bodies recurse, and every expression and
// template parses. It is static admission: a flow that passes never fails later for
// a structural reason, only for a runtime value error or a resource cap.
func (f Flow) Validate() error {
	if len(f.Steps) == 0 {
		return fault.New(fault.Terminal, "flow_no_steps", "flow: a flow needs at least one step")
	}
	seen := map[string]bool{}
	return validateSteps(f.Steps, seen)
}

func validateSteps(steps []Step, seen map[string]bool) error {
	for i := range steps {
		if err := validateStep(steps[i], seen); err != nil {
			return err
		}
	}
	return nil
}

func validateStep(s Step, seen map[string]bool) error {
	if s.ID != "" {
		if seen[s.ID] {
			return fault.New(fault.Terminal, "flow_duplicate_id", "flow: duplicate step id "+s.ID)
		}
		seen[s.ID] = true
	}
	total, matchesOp := actionBlocks(s)
	if total != 1 || !matchesOp {
		return fault.New(fault.Terminal, "flow_action_mismatch",
			fmt.Sprintf("flow: step %q must carry exactly one action block matching its op %q, found %d", stepLabel(s), s.Op, total))
	}
	switch s.Op {
	case OpHTTP:
		return validateHTTP(s.HTTP)
	case OpTransform:
		return validateTransform(s.Transform)
	case OpCondition:
		return validateCondition(s.Condition, seen)
	case OpLoop:
		return validateLoop(s.Loop, seen)
	case OpCall:
		return validateCall(s.Call)
	case OpReturn:
		return validateReturn(s.Return)
	case OpAssert:
		return validateAssert(s.Assert)
	case OpExec:
		return validateExec(s.Exec)
	case OpDependency:
		return validateDependency(s.Dependency)
	default:
		return fault.New(fault.Terminal, "flow_unknown_op", "flow: unknown op "+string(s.Op))
	}
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

func validateHTTP(a *HTTPAction) error {
	if a.URL == "" {
		return fault.New(fault.Terminal, "flow_http_no_url", "flow: http step needs a url")
	}
	if err := checkTemplate(a.URL); err != nil {
		return err
	}
	if a.Method != "" {
		if err := checkTemplate(a.Method); err != nil {
			return err
		}
	}
	for _, v := range a.Headers {
		if err := checkTemplate(v); err != nil {
			return err
		}
	}
	for _, v := range a.Query {
		if err := checkTemplate(v); err != nil {
			return err
		}
	}
	return checkBodyTemplate(a.Body)
}

func validateTransform(a *TransformAction) error {
	modes := 0
	for _, e := range []string{a.Value, a.Filter, a.Map} {
		if e != "" {
			modes++
		}
	}
	if len(a.Select) > 0 {
		modes++
	}
	if modes != 1 {
		return fault.New(fault.Terminal, "flow_transform_mode",
			"flow: transform needs exactly one of value/select/filter/map")
	}
	if a.Source != "" {
		if err := checkExpr(a.Source); err != nil {
			return err
		}
	}
	for _, e := range []string{a.Value, a.Filter, a.Map} {
		if e != "" {
			if err := checkExpr(e); err != nil {
				return err
			}
		}
	}
	for _, e := range a.Select {
		if err := checkExpr(e); err != nil {
			return err
		}
	}
	return nil
}

func validateCondition(a *ConditionAction, seen map[string]bool) error {
	if err := checkExpr(a.If); err != nil {
		return err
	}
	if err := validateSteps(a.Then, seen); err != nil {
		return err
	}
	return validateSteps(a.Else, seen)
}

func validateLoop(a *LoopAction, seen map[string]bool) error {
	if a.Over == "" && a.Count <= 0 {
		return fault.New(fault.Terminal, "flow_loop_source", "flow: loop needs over or a positive count")
	}
	if a.Over != "" {
		if err := checkExpr(a.Over); err != nil {
			return err
		}
	}
	if a.Collect != "" {
		if err := checkExpr(a.Collect); err != nil {
			return err
		}
	}
	return validateSteps(a.Body, seen)
}

func validateCall(a *CallAction) error {
	if a.Tool == "" {
		return fault.New(fault.Terminal, "flow_call_no_tool", "flow: call step needs a tool name")
	}
	if err := checkTemplate(a.Tool); err != nil {
		return err
	}
	for _, v := range a.Input {
		if err := checkDeepTemplate(v); err != nil {
			return err
		}
	}
	return nil
}

func validateReturn(a *ReturnAction) error { return checkBodyTemplate(a.Value) }

func validateAssert(a *AssertAction) error {
	if a.That == "" {
		return fault.New(fault.Terminal, "flow_assert_no_cond", "flow: assert step needs a condition")
	}
	if err := checkExpr(a.That); err != nil {
		return err
	}
	if a.Message != "" {
		return checkTemplate(a.Message)
	}
	return nil
}

func validateExec(a *ExecAction) error {
	if a.Command == "" {
		return fault.New(fault.Terminal, "flow_exec_no_command", "flow: exec step needs a command")
	}
	return checkTemplate(a.Command)
}

func validateDependency(a *DependencyAction) error {
	if a.Name == "" {
		return fault.New(fault.Terminal, "flow_dep_no_name", "flow: dependency step needs a name")
	}
	return checkTemplate(a.Name)
}

// checkExpr parses an expression and discards it, reporting a parse error as a
// terminal fault so a bad expression is caught at admission.
func checkExpr(src string) error {
	if _, err := parseExpr(src); err != nil {
		return fault.Wrap(fault.Terminal, "flow_bad_expr", err)
	}
	return nil
}

func checkTemplate(src string) error {
	if _, err := parseTemplate(src); err != nil {
		return fault.Wrap(fault.Terminal, "flow_bad_template", err)
	}
	return nil
}

func checkBodyTemplate(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fault.Wrap(fault.Terminal, "flow_bad_body", err)
	}
	return checkDeepTemplate(v)
}

// checkDeepTemplate validates every string leaf of a structured value as a
// template, the static half of deepTemplate.
func checkDeepTemplate(v any) error {
	switch x := v.(type) {
	case string:
		return checkTemplate(x)
	case []any:
		for _, e := range x {
			if err := checkDeepTemplate(e); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, e := range x {
			if err := checkDeepTemplate(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func stepLabel(s Step) string {
	if s.ID != "" {
		return s.ID
	}
	return string(s.Op)
}
