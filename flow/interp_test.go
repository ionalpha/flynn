package flow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
)

// fakeHTTP is a scripted HTTPDoer: it returns a queued response and records the
// request it was asked to perform.
type fakeHTTP struct {
	resp     HTTPResponse
	err      error
	gotReqs  []HTTPRequest
	respByID func(req HTTPRequest) HTTPResponse
}

func (f *fakeHTTP) Do(_ context.Context, req HTTPRequest) (HTTPResponse, error) {
	f.gotReqs = append(f.gotReqs, req)
	if f.err != nil {
		return HTTPResponse{}, f.err
	}
	if f.respByID != nil {
		return f.respByID(req), nil
	}
	return f.resp, nil
}

// fakeTools is a scripted ToolCaller.
type fakeTools struct {
	result   any
	gotTool  string
	gotInput json.RawMessage
}

func (f *fakeTools) Call(_ context.Context, tool string, input json.RawMessage) (any, error) {
	f.gotTool = tool
	f.gotInput = input
	return f.result, nil
}

func mustDecode(t *testing.T, raw string) Flow {
	t.Helper()
	f, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return f
}

// TestFetchExtractFilterReturn is the headline acceptance: a pure-spec flow fetches,
// extracts, filters, and returns, with no Go.
func TestFetchExtractFilterReturn(t *testing.T) {
	http := &fakeHTTP{resp: HTTPResponse{
		Status: 200,
		Body: map[string]any{"items": []any{
			map[string]any{"name": "keep", "active": true},
			map[string]any{"name": "drop", "active": false},
			map[string]any{"name": "keep2", "active": true},
		}},
	}}
	in := New(WithHTTP(http))
	flow := mustDecode(t, `{
      "steps": [
        {"id": "fetch", "op": "http", "http": {"url": "https://api/{{config.id}}"}},
        {"id": "items", "op": "transform", "transform": {"value": "steps.fetch.body.items"}},
        {"id": "active", "op": "transform", "transform": {"source": "steps.items", "filter": "it.active"}},
        {"op": "return", "return": {"value": "{{steps.active}}"}}
      ]
    }`)

	out, err := in.Run(context.Background(), flow, map[string]any{"id": "42"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The request was built from config.
	if http.gotReqs[0].URL != "https://api/42" {
		t.Fatalf("url not templated: %q", http.gotReqs[0].URL)
	}
	list, ok := out.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("expected 2 active items, got %#v", out)
	}
}

func TestSelectProjection(t *testing.T) {
	in := New()
	flow := mustDecode(t, `{
      "steps": [
        {"id": "p", "op": "transform", "transform": {
           "source": "config.user",
           "select": {"handle": "it.name", "shout": "upper(it.name)"}
        }},
        {"op": "return", "return": {"value": "{{steps.p}}"}}
      ]
    }`)
	out, err := in.Run(context.Background(), flow, map[string]any{"user": map[string]any{"name": "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"handle": "ada", "shout": "ADA"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %#v", out)
	}
}

func TestConditionBranches(t *testing.T) {
	flow := mustDecode(t, `{
      "steps": [
        {"op": "condition", "condition": {
           "if": "config.n > 10",
           "then": [{"op": "return", "return": {"value": "big"}}],
           "else": [{"op": "return", "return": {"value": "small"}}]
        }}
      ]
    }`)
	in := New()
	for _, c := range []struct {
		n    float64
		want string
	}{{5, "small"}, {20, "big"}} {
		out, err := in.Run(context.Background(), flow, map[string]any{"n": c.n})
		if err != nil {
			t.Fatal(err)
		}
		if out != c.want {
			t.Fatalf("n=%v got %v want %v", c.n, out, c.want)
		}
	}
}

func TestLoopCollect(t *testing.T) {
	flow := mustDecode(t, `{
      "steps": [
        {"id": "doubled", "op": "loop", "loop": {
           "over": "config.nums", "as": "n", "collect": "n * 2",
           "body": []
        }},
        {"op": "return", "return": {"value": "{{steps.doubled}}"}}
      ]
    }`)
	out, err := New().Run(context.Background(), flow, map[string]any{"nums": []any{float64(1), float64(2), float64(3)}})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{float64(2), float64(4), float64(6)}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %#v", out)
	}
}

func TestCallStep(t *testing.T) {
	tools := &fakeTools{result: map[string]any{"ok": true}}
	flow := mustDecode(t, `{
      "steps": [
        {"id": "r", "op": "call", "call": {"tool": "notify", "input": {"msg": "hi {{config.who}}"}}},
        {"op": "return", "return": {"value": "{{steps.r}}"}}
      ]
    }`)
	out, err := New(WithTools(tools)).Run(context.Background(), flow, map[string]any{"who": "ada"})
	if err != nil {
		t.Fatal(err)
	}
	if tools.gotTool != "notify" {
		t.Fatalf("tool %q", tools.gotTool)
	}
	var gotInput map[string]any
	if err := json.Unmarshal(tools.gotInput, &gotInput); err != nil {
		t.Fatal(err)
	}
	if gotInput["msg"] != "hi ada" {
		t.Fatalf("input not templated: %v", gotInput)
	}
	if !reflect.DeepEqual(out, map[string]any{"ok": true}) {
		t.Fatalf("got %#v", out)
	}
}

// TestHTTPStepFailsClosedWithoutPort proves an http step refuses when no transport
// is configured, rather than silently doing nothing.
func TestHTTPStepFailsClosedWithoutPort(t *testing.T) {
	flow := mustDecode(t, `{"steps":[{"op":"http","http":{"url":"https://x"}}]}`)
	_, err := New().Run(context.Background(), flow, nil)
	if err == nil {
		t.Fatal("expected a fail-closed error for an http step with no transport")
	}
}

func TestMaxStepsCap(t *testing.T) {
	// A loop of 100 iterations with a step in the body blows a tiny step cap.
	flow := mustDecode(t, `{
      "steps": [{"op": "loop", "loop": {
        "count": 100,
        "body": [{"op": "transform", "transform": {"value": "1"}}]
      }}]
    }`)
	_, err := New(WithLimits(Limits{MaxSteps: 10})).Run(context.Background(), flow, nil)
	if err == nil || !contains(err.Error(), "max steps") {
		t.Fatalf("expected max-steps cap, got %v", err)
	}
}

func TestMaxLoopIterationsCap(t *testing.T) {
	flow := mustDecode(t, `{
      "steps": [{"op": "loop", "loop": {"count": 100, "body": []}}]
    }`)
	_, err := New(WithLimits(Limits{MaxLoopIterations: 5})).Run(context.Background(), flow, nil)
	if err == nil || !contains(err.Error(), "loop iteration cap") {
		t.Fatalf("expected loop cap, got %v", err)
	}
}

// TestTimeoutCap uses a manual clock and an HTTPDoer that advances it, so the cap
// trips deterministically with no real sleeping.
func TestTimeoutCap(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0).UTC())
	http := &fakeHTTP{respByID: func(_ HTTPRequest) HTTPResponse {
		clk.Advance(time.Hour) // each request pushes time past the cap
		return HTTPResponse{Status: 200}
	}}
	flow := mustDecode(t, `{
      "steps": [
        {"op": "http", "http": {"url": "https://x"}},
        {"op": "http", "http": {"url": "https://y"}}
      ]
    }`)
	_, err := New(WithHTTP(http), WithClock(clk), WithLimits(Limits{TimeoutMillis: 1000})).
		Run(context.Background(), flow, nil)
	if err == nil || !contains(err.Error(), "time cap") {
		t.Fatalf("expected timeout cap, got %v", err)
	}
}

func TestMaxPayloadCap(t *testing.T) {
	http := &fakeHTTP{resp: HTTPResponse{Status: 200, Raw: make([]byte, 100)}}
	flow := mustDecode(t, `{"steps":[{"op":"http","http":{"url":"https://x"}}]}`)
	_, err := New(WithHTTP(http), WithLimits(Limits{MaxPayloadBytes: 10})).Run(context.Background(), flow, nil)
	if err == nil || !contains(err.Error(), "payload cap") {
		t.Fatalf("expected payload cap, got %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flow := mustDecode(t, `{"steps":[{"op":"return","return":{"value":"1"}}]}`)
	_, err := New().Run(ctx, flow, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	// Cancellation is classified Cancelled, distinct from a terminal failure, so a
	// caller can tell an operator stop apart from a genuine error.
	if cls := fault.Classify(err); cls != fault.Cancelled {
		t.Fatalf("expected Cancelled class, got %q", cls)
	}
}

// TestHugeCountDoesNotAllocate proves a fixed Count is iterated by index, not
// materialised: a billion-iteration loop trips the iteration cap promptly instead
// of allocating a billion-element slice first. If the loop pre-built the slice this
// test would OOM rather than fail with the cap error.
func TestHugeCountDoesNotAllocate(t *testing.T) {
	flow := mustDecode(t, `{
      "steps": [{"id": "x", "op": "loop", "loop": {
        "count": 1000000000, "collect": "index", "body": []
      }}]
    }`)
	_, err := New(WithLimits(Limits{MaxLoopIterations: 5})).Run(context.Background(), flow, nil)
	if err == nil || !contains(err.Error(), "loop iteration cap") {
		t.Fatalf("expected loop cap, got %v", err)
	}
}

// tickingClock advances a fixed step on every read, so a loop body with no inner
// steps still moves time forward and a per-iteration deadline check can observe it.
type tickingClock struct {
	t    time.Time
	step time.Duration
}

func (c *tickingClock) Now() time.Time {
	now := c.t
	c.t = c.t.Add(c.step)
	return now
}

// TestEmptyBodyLoopObservesTimeout proves the timeout cap is checked inside a loop,
// not only between top-level steps: a loop with an empty body must still stop on the
// deadline rather than running to its iteration count.
func TestEmptyBodyLoopObservesTimeout(t *testing.T) {
	clk := &tickingClock{t: time.Unix(0, 0).UTC(), step: time.Second}
	flow := mustDecode(t, `{
      "steps": [{"op": "loop", "loop": {"count": 100, "body": []}}]
    }`)
	_, err := New(WithClock(clk), WithLimits(Limits{TimeoutMillis: 5000, MaxLoopIterations: 1000})).
		Run(context.Background(), flow, nil)
	if err == nil || !contains(err.Error(), "time cap") {
		t.Fatalf("expected timeout inside the loop, got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
