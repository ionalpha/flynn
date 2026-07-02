package flow

import (
	"encoding/json"
	"fmt"

	"github.com/ionalpha/flynn/fault"
)

// This file is the compile pass: it checks a decoded Flow's structure and parses
// every expression, template, and templated body into its evaluated form exactly
// once. Decode runs it and keeps the result on the Flow, so execution never lexes
// or parses; a transform over N elements evaluates one parsed AST N times instead
// of parsing the identical source N times. The compiled tree is read-only after
// compile, so one Flow value can run concurrently.

// compiledFlow is the executable form of a Flow: the same step tree with every
// expression and template parsed and every JSON body pre-decoded.
type compiledFlow struct {
	steps []compiledStep
}

// compiledStep mirrors Step with its action block in compiled form. Exactly one
// action pointer is set, matching op; compileStep enforces that.
type compiledStep struct {
	id string
	op Op

	http       *compiledHTTP
	transform  *compiledTransform
	condition  *compiledCondition
	loop       *compiledLoop
	call       *compiledCall
	ret        *compiledReturn
	assert     *compiledAssert
	exec       *compiledExec
	dependency *compiledDependency
	confirm    *compiledConfirm
	secret     *compiledSecret
}

type compiledHTTP struct {
	method  *template // nil means the default GET, no rendering needed
	url     *template
	headers map[string]*template
	query   map[string]*template
	body    *compiledValue // nil when the action has no body
}

type compiledTransform struct {
	source  node // nil when the action has no source
	value   node
	sel     map[string]node
	filter  node
	mapExpr node
}

type compiledCondition struct {
	ifExpr node
	then   []compiledStep
	els    []compiledStep
}

type compiledLoop struct {
	over    node // nil in count mode
	count   int
	as      string // normalized: never empty
	body    []compiledStep
	collect node // nil when the loop collects nothing
}

type compiledCall struct {
	tool  *template
	input map[string]*compiledValue
}

type compiledReturn struct {
	value *compiledValue // nil when the return carries no value
}

type compiledAssert struct {
	that    node
	thatSrc string // the source text, for the default failure message
	message *template
}

type compiledExec struct {
	command      *template
	allowNonzero bool
}

type compiledDependency struct {
	name *template
}

type compiledConfirm struct {
	message *template
}

type compiledSecret struct {
	ref    *template
	sink   *template
	target map[string]*template
}

// compileFlow checks the flow's structure and parses everything, returning the
// executable tree. It is static admission: a flow that compiles never fails later
// for a structural or parse reason, only for a runtime value error or a resource
// cap.
func compileFlow(f Flow) (*compiledFlow, error) {
	if len(f.Steps) == 0 {
		return nil, fault.New(fault.Terminal, "flow_no_steps", "flow: a flow needs at least one step")
	}
	seen := map[string]bool{}
	steps, err := compileSteps(f.Steps, seen)
	if err != nil {
		return nil, err
	}
	return &compiledFlow{steps: steps}, nil
}

func compileSteps(steps []Step, seen map[string]bool) ([]compiledStep, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	out := make([]compiledStep, len(steps))
	for i := range steps {
		cs, err := compileStep(steps[i], seen)
		if err != nil {
			return nil, err
		}
		out[i] = cs
	}
	return out, nil
}

func compileStep(s Step, seen map[string]bool) (compiledStep, error) {
	if s.ID != "" {
		if seen[s.ID] {
			return compiledStep{}, fault.New(fault.Terminal, "flow_duplicate_id", "flow: duplicate step id "+s.ID)
		}
		seen[s.ID] = true
	}
	total, matchesOp := actionBlocks(s)
	if total != 1 || !matchesOp {
		return compiledStep{}, fault.New(fault.Terminal, "flow_action_mismatch",
			fmt.Sprintf("flow: step %q must carry exactly one action block matching its op %q, found %d", stepLabel(s), s.Op, total))
	}
	cs := compiledStep{id: s.ID, op: s.Op}
	var err error
	switch s.Op {
	case OpHTTP:
		cs.http, err = compileHTTP(s.HTTP)
	case OpTransform:
		cs.transform, err = compileTransform(s.Transform)
	case OpCondition:
		cs.condition, err = compileCondition(s.Condition, seen)
	case OpLoop:
		cs.loop, err = compileLoop(s.Loop, seen)
	case OpCall:
		cs.call, err = compileCall(s.Call)
	case OpReturn:
		cs.ret, err = compileReturn(s.Return)
	case OpAssert:
		cs.assert, err = compileAssert(s.Assert)
	case OpExec:
		cs.exec, err = compileExec(s.Exec)
	case OpDependency:
		cs.dependency, err = compileDependency(s.Dependency)
	case OpConfirm:
		cs.confirm, err = compileConfirm(s.Confirm)
	case OpSecret:
		cs.secret, err = compileSecret(s.Secret)
	default:
		return compiledStep{}, fault.New(fault.Terminal, "flow_unknown_op", "flow: unknown op "+string(s.Op))
	}
	if err != nil {
		return compiledStep{}, err
	}
	return cs, nil
}

func compileHTTP(a *HTTPAction) (*compiledHTTP, error) {
	if a.URL == "" {
		return nil, fault.New(fault.Terminal, "flow_http_no_url", "flow: http step needs a url")
	}
	c := &compiledHTTP{}
	var err error
	if c.url, err = compileTemplate(a.URL); err != nil {
		return nil, err
	}
	if a.Method != "" {
		if c.method, err = compileTemplate(a.Method); err != nil {
			return nil, err
		}
	}
	if c.headers, err = compileTemplateMap(a.Headers); err != nil {
		return nil, err
	}
	if c.query, err = compileTemplateMap(a.Query); err != nil {
		return nil, err
	}
	if c.body, err = compileBody(a.Body); err != nil {
		return nil, err
	}
	return c, nil
}

func compileTransform(a *TransformAction) (*compiledTransform, error) {
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
		return nil, fault.New(fault.Terminal, "flow_transform_mode",
			"flow: transform needs exactly one of value/select/filter/map")
	}
	c := &compiledTransform{}
	var err error
	if a.Source != "" {
		if c.source, err = compileExpr(a.Source); err != nil {
			return nil, err
		}
	}
	if a.Value != "" {
		if c.value, err = compileExpr(a.Value); err != nil {
			return nil, err
		}
	}
	if a.Filter != "" {
		if c.filter, err = compileExpr(a.Filter); err != nil {
			return nil, err
		}
	}
	if a.Map != "" {
		if c.mapExpr, err = compileExpr(a.Map); err != nil {
			return nil, err
		}
	}
	if len(a.Select) > 0 {
		c.sel = make(map[string]node, len(a.Select))
		for k, e := range a.Select {
			if c.sel[k], err = compileExpr(e); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

func compileCondition(a *ConditionAction, seen map[string]bool) (*compiledCondition, error) {
	c := &compiledCondition{}
	var err error
	if c.ifExpr, err = compileExpr(a.If); err != nil {
		return nil, err
	}
	if c.then, err = compileSteps(a.Then, seen); err != nil {
		return nil, err
	}
	if c.els, err = compileSteps(a.Else, seen); err != nil {
		return nil, err
	}
	return c, nil
}

func compileLoop(a *LoopAction, seen map[string]bool) (*compiledLoop, error) {
	if a.Over == "" && a.Count <= 0 {
		return nil, fault.New(fault.Terminal, "flow_loop_source", "flow: loop needs over or a positive count")
	}
	c := &compiledLoop{count: a.Count, as: orDefault(a.As, "item")}
	var err error
	if a.Over != "" {
		if c.over, err = compileExpr(a.Over); err != nil {
			return nil, err
		}
	}
	if a.Collect != "" {
		if c.collect, err = compileExpr(a.Collect); err != nil {
			return nil, err
		}
	}
	if c.body, err = compileSteps(a.Body, seen); err != nil {
		return nil, err
	}
	return c, nil
}

func compileCall(a *CallAction) (*compiledCall, error) {
	if a.Tool == "" {
		return nil, fault.New(fault.Terminal, "flow_call_no_tool", "flow: call step needs a tool name")
	}
	c := &compiledCall{}
	var err error
	if c.tool, err = compileTemplate(a.Tool); err != nil {
		return nil, err
	}
	if len(a.Input) > 0 {
		c.input = make(map[string]*compiledValue, len(a.Input))
		for k, v := range a.Input {
			if c.input[k], err = compileValue(v); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

func compileReturn(a *ReturnAction) (*compiledReturn, error) {
	v, err := compileBody(a.Value)
	if err != nil {
		return nil, err
	}
	return &compiledReturn{value: v}, nil
}

func compileAssert(a *AssertAction) (*compiledAssert, error) {
	if a.That == "" {
		return nil, fault.New(fault.Terminal, "flow_assert_no_cond", "flow: assert step needs a condition")
	}
	c := &compiledAssert{thatSrc: a.That}
	var err error
	if c.that, err = compileExpr(a.That); err != nil {
		return nil, err
	}
	if a.Message != "" {
		if c.message, err = compileTemplate(a.Message); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func compileExec(a *ExecAction) (*compiledExec, error) {
	if a.Command == "" {
		return nil, fault.New(fault.Terminal, "flow_exec_no_command", "flow: exec step needs a command")
	}
	cmd, err := compileTemplate(a.Command)
	if err != nil {
		return nil, err
	}
	return &compiledExec{command: cmd, allowNonzero: a.AllowNonzero}, nil
}

func compileDependency(a *DependencyAction) (*compiledDependency, error) {
	if a.Name == "" {
		return nil, fault.New(fault.Terminal, "flow_dep_no_name", "flow: dependency step needs a name")
	}
	name, err := compileTemplate(a.Name)
	if err != nil {
		return nil, err
	}
	return &compiledDependency{name: name}, nil
}

func compileConfirm(a *ConfirmAction) (*compiledConfirm, error) {
	if a.Message == "" {
		return nil, fault.New(fault.Terminal, "flow_confirm_no_message", "flow: confirm step needs a message")
	}
	msg, err := compileTemplate(a.Message)
	if err != nil {
		return nil, err
	}
	return &compiledConfirm{message: msg}, nil
}

func compileSecret(a *SecretAction) (*compiledSecret, error) {
	if a.Ref == "" {
		return nil, fault.New(fault.Terminal, "flow_secret_no_ref", "flow: secret step needs a ref")
	}
	if a.Sink == "" {
		return nil, fault.New(fault.Terminal, "flow_secret_no_sink", "flow: secret step needs a sink")
	}
	c := &compiledSecret{}
	var err error
	if c.ref, err = compileTemplate(a.Ref); err != nil {
		return nil, err
	}
	if c.sink, err = compileTemplate(a.Sink); err != nil {
		return nil, err
	}
	if c.target, err = compileTemplateMap(a.Target); err != nil {
		return nil, err
	}
	return c, nil
}

// compileExpr parses an expression, reporting a parse error as a terminal fault so
// a bad expression is rejected at admission.
func compileExpr(src string) (node, error) {
	n, err := parseExpr(src)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_bad_expr", err)
	}
	return n, nil
}

func compileTemplate(src string) (*template, error) {
	t, err := parseTemplate(src)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_bad_template", err)
	}
	return t, nil
}

func compileTemplateMap(m map[string]string) (map[string]*template, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]*template, len(m))
	for k, v := range m {
		t, err := compileTemplate(v)
		if err != nil {
			return nil, err
		}
		out[k] = t
	}
	return out, nil
}

// compileBody decodes a JSON body and compiles it, so execution renders the
// pre-decoded structure instead of unmarshalling and re-parsing per run. An empty
// body compiles to nil.
func compileBody(raw json.RawMessage) (*compiledValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_bad_body", err)
	}
	return compileValue(v)
}

// compiledValue is a decoded JSON value with every string leaf compiled as a
// template, the parsed form deep templating renders from. Object keys are not
// templated (only values), so the shape of a body stays fixed while its data is
// filled in.
type compiledValue struct {
	tmpl *template // set for a string leaf
	list []*compiledValue
	obj  map[string]*compiledValue
	lit  any // any other leaf (number, bool, nil)
}

func compileValue(v any) (*compiledValue, error) {
	switch x := v.(type) {
	case string:
		t, err := compileTemplate(x)
		if err != nil {
			return nil, err
		}
		return &compiledValue{tmpl: t}, nil
	case []any:
		list := make([]*compiledValue, len(x))
		for i, e := range x {
			c, err := compileValue(e)
			if err != nil {
				return nil, err
			}
			list[i] = c
		}
		return &compiledValue{list: list}, nil
	case map[string]any:
		obj := make(map[string]*compiledValue, len(x))
		for k, e := range x {
			c, err := compileValue(e)
			if err != nil {
				return nil, err
			}
			obj[k] = c
		}
		return &compiledValue{obj: obj}, nil
	default:
		return &compiledValue{lit: v}, nil
	}
}

// render evaluates the value against a scope, preserving the surrounding
// structure. A string leaf that is a whole-template single expression takes the
// expression's typed value, so a body field can become a list or number, not only
// a string.
func (c *compiledValue) render(s *scope) (any, error) {
	switch {
	case c.tmpl != nil:
		return c.tmpl.renderValue(s)
	case c.list != nil:
		out := make([]any, len(c.list))
		for i, e := range c.list {
			v, err := e.render(s)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case c.obj != nil:
		out := make(map[string]any, len(c.obj))
		for k, e := range c.obj {
			v, err := e.render(s)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	default:
		return c.lit, nil
	}
}
