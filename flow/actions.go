package flow

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ionalpha/flynn/fault"
)

// execHTTP renders and performs one request, returning a map output of
// {status, headers, body} so later steps reference steps.<id>.body and .status by
// path. The response payload is checked against the cap before it is exposed.
func (r *run) execHTTP(ctx context.Context, a *HTTPAction, s *scope) (any, error) {
	if r.in.http == nil {
		return nil, fault.New(fault.Terminal, "flow_no_http", "flow: http step but no transport is configured")
	}
	method, err := renderTemplateString(orDefault(a.Method, "GET"), s)
	if err != nil {
		return nil, templErr(err)
	}
	rawURL, err := renderTemplateString(a.URL, s)
	if err != nil {
		return nil, templErr(err)
	}
	headers, err := renderStringMap(a.Headers, s)
	if err != nil {
		return nil, err
	}
	query, err := renderStringMap(a.Query, s)
	if err != nil {
		return nil, err
	}
	var body []byte
	if len(a.Body) > 0 {
		v, err := renderBody(a.Body, s)
		if err != nil {
			return nil, err
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

// execTransform reshapes a source value. The source (if any) is bound as "it" in a
// child scope so the mode expressions read the element under transformation.
func (r *run) execTransform(a *TransformAction, s *scope) (any, error) {
	var input any
	if a.Source != "" {
		v, err := evalExpr(a.Source, s)
		if err != nil {
			return nil, err
		}
		input = v
	}
	inner := s.child(map[string]any{"it": input})

	switch {
	case a.Value != "":
		return evalExpr(a.Value, inner)
	case len(a.Select) > 0:
		out := make(map[string]any, len(a.Select))
		for k, expr := range a.Select {
			v, err := evalExpr(expr, inner)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case a.Filter != "":
		list, ok := input.([]any)
		if !ok {
			return nil, fault.New(fault.Terminal, "flow_filter_not_list", "flow: filter source must be a list")
		}
		out := make([]any, 0, len(list))
		for _, e := range list {
			keep, err := evalExpr(a.Filter, s.child(map[string]any{"it": e}))
			if err != nil {
				return nil, err
			}
			if truthy(keep) {
				out = append(out, e)
			}
		}
		return out, nil
	case a.Map != "":
		list, ok := input.([]any)
		if !ok {
			return nil, fault.New(fault.Terminal, "flow_map_not_list", "flow: map source must be a list")
		}
		out := make([]any, len(list))
		for i, e := range list {
			v, err := evalExpr(a.Map, s.child(map[string]any{"it": e}))
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

// execCondition evaluates the predicate and runs the matching branch, propagating a
// return out of the branch.
func (r *run) execCondition(ctx context.Context, a *ConditionAction, s *scope) error {
	cond, err := evalExpr(a.If, s)
	if err != nil {
		return err
	}
	branch := a.Else
	if truthy(cond) {
		branch = a.Then
	}
	_, err = r.execSteps(ctx, branch, s)
	return err
}

// execLoop iterates a body over a list or a fixed count, binding the element (as
// As, default "item") and the zero-based "index" in each iteration's scope. Every
// iteration is charged against the loop budget and re-checks the time and
// cancellation caps, so a loop with an empty or collect-only body still stops on a
// deadline or a cancelled context. When Collect is set, its value per iteration is
// gathered into the returned list. A fixed Count is iterated by index rather than
// materialised, so a large declared count cannot allocate before the loop cap
// applies.
func (r *run) execLoop(ctx context.Context, a *LoopAction, s *scope) (any, error) {
	overMode := a.Over != ""
	var list []any
	n := a.Count
	if overMode {
		v, err := evalExpr(a.Over, s)
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

	as := orDefault(a.As, "item")
	var collected []any
	if a.Collect != "" {
		// The loop cap bounds how many values can ever be collected, so the capacity
		// is bounded too even when a huge count or list is declared.
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
		iter := s.child(map[string]any{as: item, "index": float64(i)})
		returned, err := r.execSteps(ctx, a.Body, iter)
		if err != nil {
			return nil, err
		}
		if returned {
			// The flow is stopping; do not collect or run Collect against this
			// partially executed iteration's scope.
			break
		}
		if a.Collect != "" {
			v, err := evalExpr(a.Collect, iter)
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

// execCall invokes another tool or extension and returns its result.
func (r *run) execCall(ctx context.Context, a *CallAction, s *scope) (any, error) {
	if r.in.tools == nil {
		return nil, fault.New(fault.Terminal, "flow_no_tools", "flow: call step but no tool caller is configured")
	}
	tool, err := renderTemplateString(a.Tool, s)
	if err != nil {
		return nil, templErr(err)
	}
	input := map[string]any{}
	for k, v := range a.Input {
		rendered, err := deepTemplate(v, s)
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

// evalExpr parses and evaluates an expression against a scope. Parse errors are
// already caught at admission, so a parse failure here is still surfaced as a
// terminal fault for safety.
func evalExpr(src string, s *scope) (any, error) {
	n, err := parseExpr(src)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_bad_expr", err)
	}
	v, err := n.eval(s)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "flow_eval", err)
	}
	return v, nil
}

func renderStringMap(m map[string]string, s *scope) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		rendered, err := renderTemplateString(v, s)
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
