package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// execHTTP performs one request and returns {status, headers, body}, so later steps
// read steps.<id>.body and .status. The payload is checked against the cap before
// it is exposed.
func (r *run) execHTTP(ctx context.Context, a *compiledHTTP, s *scope) (any, error) {
	if r.in.http == nil {
		return nil, fault.New(fault.Terminal, "flow_no_http", "flow: http step but no transport is configured")
	}
	method := "GET"
	if a.method != nil {
		var err error
		if method, err = a.method.renderString(s); err != nil {
			return nil, templErr(err)
		}
	}
	rawURL, err := a.url.renderString(s)
	if err != nil {
		return nil, templErr(err)
	}
	headers, err := renderStringMap(a.headers, s)
	if err != nil {
		return nil, err
	}
	query, err := renderStringMap(a.query, s)
	if err != nil {
		return nil, err
	}
	var body []byte
	if a.body != nil {
		v, err := a.body.render(s)
		if err != nil {
			return nil, templErr(err)
		}
		body, err = json.Marshal(v)
		if err != nil {
			return nil, fault.Wrap(fault.Terminal, "flow_body_encode", err)
		}
	}

	resp, err := r.in.http.Do(ctx, HTTPRequest{
		Method:  method,
		URL:     rawURL,
		Headers: headers,
		Query:   query,
		Body:    body,
	})
	if err != nil {
		return nil, err
	}
	if r.limits.MaxPayloadBytes > 0 && len(resp.Raw) > r.limits.MaxPayloadBytes {
		return nil, fault.New(fault.Terminal, "flow_max_payload", "flow: response exceeded payload cap")
	}
	return map[string]any{
		"status":  float64(resp.Status),
		"headers": stringMapToAny(resp.Headers),
		"body":    resp.Body,
	}, nil
}

// execTransform binds the source as "it" in a child scope, so mode expressions read
// the element being transformed.
func (r *run) execTransform(a *compiledTransform, s *scope) (any, error) {
	var input any
	if a.source != nil {
		v, err := evalNode(a.source, s)
		if err != nil {
			return nil, err
		}
		input = v
	}
	inner := s.child(map[string]any{"it": input})

	switch {
	case a.value != nil:
		return evalNode(a.value, inner)
	case len(a.sel) > 0:
		out := make(map[string]any, len(a.sel))
		for k, expr := range a.sel {
			v, err := evalNode(expr, inner)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case a.filter != nil:
		list, ok := input.([]any)
		if !ok {
			return nil, fault.New(fault.Terminal, "flow_filter_not_list", "flow: filter source must be a list")
		}
		out := make([]any, 0, len(list))
		for _, e := range list {
			keep, err := evalNode(a.filter, s.child(map[string]any{"it": e}))
			if err != nil {
				return nil, err
			}
			if truthy(keep) {
				out = append(out, e)
			}
		}
		return out, nil
	case a.mapExpr != nil:
		list, ok := input.([]any)
		if !ok {
			return nil, fault.New(fault.Terminal, "flow_map_not_list", "flow: map source must be a list")
		}
		out := make([]any, len(list))
		for i, e := range list {
			v, err := evalNode(a.mapExpr, s.child(map[string]any{"it": e}))
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	default:
		return nil, fault.New(fault.Terminal, "flow_transform_mode", "flow: transform has no mode")
	}
}

// execCondition runs the matching branch, propagating a return out of it.
func (r *run) execCondition(ctx context.Context, a *compiledCondition, s *scope) error {
	cond, err := evalNode(a.ifExpr, s)
	if err != nil {
		return err
	}
	branch := a.els
	if truthy(cond) {
		branch = a.then
	}
	_, err = r.execSteps(ctx, branch, s)
	return err
}

// execLoop runs the body over a list or a count, binding the element (As, default
// "item") and the zero-based "index". Each iteration charges the loop budget and
// rechecks the time and cancellation caps, so an empty or collect-only body still
// stops on a deadline. A Count is iterated by index, never materialized, so a huge
// count cannot allocate before the loop cap applies.
func (r *run) execLoop(ctx context.Context, a *compiledLoop, s *scope) (any, error) {
	overMode := a.over != nil
	var list []any
	n := a.count
	if overMode {
		v, err := evalNode(a.over, s)
		if err != nil {
			return nil, err
		}
		var ok bool
		list, ok = v.([]any)
		if !ok {
			return nil, fault.New(fault.Terminal, "flow_loop_not_list", "flow: loop over must yield a list")
		}
		n = len(list)
	}

	var collected []any
	if a.collect != nil {
		// The loop cap bounds how many values are collected, so it bounds capacity
		// too.
		collected = make([]any, 0, minInt(n, r.limits.MaxLoopIterations))
	}

	for i := range n {
		r.loops++
		if r.loops > r.limits.MaxLoopIterations {
			return nil, fault.New(fault.Terminal, "flow_max_loops", "flow: exceeded loop iteration cap")
		}
		if err := r.checkDeadline(ctx); err != nil {
			return nil, err
		}
		item := any(float64(i))
		if overMode {
			item = list[i]
		}
		iter := s.child(map[string]any{a.as: item, "index": float64(i)})
		returned, err := r.execSteps(ctx, a.body, iter)
		if err != nil {
			return nil, err
		}
		if returned {
			// The flow is stopping; do not run Collect against a partial iteration.
			break
		}
		if a.collect != nil {
			v, err := evalNode(a.collect, iter)
			if err != nil {
				return nil, err
			}
			collected = append(collected, v)
		}
	}
	return collected, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (r *run) execCall(ctx context.Context, a *compiledCall, s *scope) (any, error) {
	if r.in.tools == nil {
		return nil, fault.New(fault.Terminal, "flow_no_tools", "flow: call step but no tool caller is configured")
	}
	tool, err := a.tool.renderString(s)
	if err != nil {
		return nil, templErr(err)
	}
	input := map[string]any{}
	for k, v := range a.input {
		rendered, err := v.render(s)
		if err != nil {
			return nil, templErr(err)
		}
		input[k] = rendered
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_call_encode", err)
	}
	return r.in.tools.Call(ctx, tool, raw)
}

// execAssert stops the flow with a terminal fault when the condition is not truthy,
// so a verification step fails loudly.
func (r *run) execAssert(a *compiledAssert, s *scope) error {
	v, err := evalNode(a.that, s)
	if err != nil {
		return err
	}
	if truthy(v) {
		return nil
	}
	msg := "flow: assertion failed: " + a.thatSrc
	if a.message != nil {
		if rendered, rerr := a.message.renderString(s); rerr == nil {
			msg = rendered
		}
	}
	return fault.New(fault.Terminal, "flow_assertion", msg)
}

// execExec runs a command through the injected sandbox runner and returns
// {exitCode, output}. A nonzero exit fails the step unless the action allows it.
func (r *run) execExec(ctx context.Context, a *compiledExec, s *scope) (any, error) {
	if r.in.exec == nil {
		return nil, fault.New(fault.Terminal, "flow_no_exec", "flow: exec step but no runner is configured")
	}
	cmd, err := a.command.renderString(s)
	if err != nil {
		return nil, templErr(err)
	}
	r.observe(StepEvent{Phase: StepBegin, Op: OpExec, Detail: cmd})
	res, err := r.in.exec.Exec(ctx, ExecRequest{Command: cmd})
	if err != nil {
		r.observe(StepEvent{Phase: StepEnd, Op: OpExec, Detail: cmd, Err: err})
		return nil, err
	}
	r.observe(StepEvent{Phase: StepEnd, Op: OpExec, Detail: cmd, ExitCode: res.ExitCode, Output: res.Output})
	if res.ExitCode != 0 && !a.allowNonzero {
		return nil, execFailedFault(cmd, res)
	}
	return map[string]any{"exitCode": float64(res.ExitCode), "output": res.Output}, nil
}

// maxFaultOutputBytes bounds the command output a failure fault carries.
const maxFaultOutputBytes = 2000

// execFailedFault builds the fault for a disallowed nonzero exit, carrying the
// command and the tail of its output, where a failure reason is usually printed, so
// the error is diagnosable alone.
func execFailedFault(cmd string, res ExecResult) error {
	msg := fmt.Sprintf("flow: command exited %d: %s", res.ExitCode, cmd)
	if out := strings.TrimSpace(res.Output); out != "" {
		if len(out) > maxFaultOutputBytes {
			out = "...\n" + out[len(out)-maxFaultOutputBytes:]
		}
		msg += "\n" + out
	}
	return fault.New(fault.Terminal, "flow_exec_failed", msg)
}

// execDependency resolves the named program and returns {path}, so a later exec
// step can run it.
func (r *run) execDependency(ctx context.Context, a *compiledDependency, s *scope) (any, error) {
	if r.in.deps == nil {
		return nil, fault.New(fault.Terminal, "flow_no_deps", "flow: dependency step but no resolver is configured")
	}
	name, err := a.name.renderString(s)
	if err != nil {
		return nil, templErr(err)
	}
	r.observe(StepEvent{Phase: StepBegin, Op: OpDependency, Detail: name})
	path, err := r.in.deps.Resolve(ctx, name)
	if err != nil {
		r.observe(StepEvent{Phase: StepEnd, Op: OpDependency, Detail: name, Err: err})
		return nil, err
	}
	r.observe(StepEvent{Phase: StepEnd, Op: OpDependency, Detail: name})
	return map[string]any{"path": path}, nil
}

// execConfirm shows the rendered message and waits for approval. A declined or
// unanswerable confirmation is an error, so the flow never proceeds without consent.
func (r *run) execConfirm(ctx context.Context, a *compiledConfirm, s *scope) error {
	if r.in.confirm == nil {
		return fault.New(fault.Terminal, "flow_no_confirm", "flow: confirm step but no prompter is configured")
	}
	msg, err := a.message.renderString(s)
	if err != nil {
		return templErr(err)
	}
	return r.in.confirm.Confirm(ctx, msg)
}

// execSecret materializes the referenced secret into the named sink. It renders
// only reference, sink and target, and reports the step by reference and sink. The
// sink resolves and delivers the value, so it never enters the flow, the command,
// the step output or a log.
func (r *run) execSecret(ctx context.Context, a *compiledSecret, s *scope) error {
	if r.in.sink == nil {
		return fault.New(fault.Terminal, "flow_no_sink", "flow: secret step but no credential sink is configured")
	}
	ref, err := a.ref.renderString(s)
	if err != nil {
		return templErr(err)
	}
	sink, err := a.sink.renderString(s)
	if err != nil {
		return templErr(err)
	}
	target, err := renderStringMap(a.target, s)
	if err != nil {
		return err
	}
	subject := sink + ":" + ref
	r.observe(StepEvent{Phase: StepBegin, Op: OpSecret, Detail: subject})
	if err := r.in.sink.Put(ctx, sink, ref, target); err != nil {
		r.observe(StepEvent{Phase: StepEnd, Op: OpSecret, Detail: subject, Err: err})
		return err
	}
	r.observe(StepEvent{Phase: StepEnd, Op: OpSecret, Detail: subject})
	return nil
}

// evalNode evaluates a compiled expression, returning an evaluation error as a
// terminal fault.
func evalNode(n node, s *scope) (any, error) {
	v, err := n.eval(s)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_eval", err)
	}
	return v, nil
}

func renderStringMap(m map[string]*template, s *scope) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, t := range m {
		rendered, err := t.renderString(s)
		if err != nil {
			return nil, templErr(err)
		}
		out[k] = rendered
	}
	return out, nil
}

func stringMapToAny(m map[string]string) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func templErr(err error) error {
	var fe *fault.Error
	if errors.As(err, &fe) {
		return err
	}
	return fault.Wrap(fault.Terminal, "flow_template", err)
}
