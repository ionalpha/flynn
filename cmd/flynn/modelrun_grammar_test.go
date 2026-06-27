package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/inference/serve"
	"github.com/ionalpha/flynn/llm"
)

// sentGrammar stands a stub OpenAI-compatible server up, drives one tool-using request through a
// local client built for the given plan, and reports whether the request carried a grammar field.
// It is how a test sees what the plan actually put on the wire to the local runtime.
func sentGrammar(t *testing.T, plan harness.Plan) (string, bool) {
	t.Helper()
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client := localModelClient(serve.Endpoint{BaseURL: srv.URL}, "local:model", plan)
	req := llm.Request{
		Messages: []llm.Message{llm.Text(llm.RoleUser, "read the file")},
		Tools: []llm.Tool{{
			Name:        "read",
			Description: "read a file",
			InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}},"additionalProperties":false}`),
		}},
	}
	if _, err := client.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	g, ok := body["grammar"].(string)
	return g, ok
}

// TestLocalClientConstrainsWhenPlanDemands proves the moat-critical wiring: a plan that does not
// trust the model's tool calls makes the local client constrain decoding to a grammar, so a
// malformed call is structurally impossible.
func TestLocalClientConstrainsWhenPlanDemands(t *testing.T) {
	if g, ok := sentGrammar(t, harness.Plan{ConstrainToolCalls: true}); !ok || g == "" {
		t.Fatal("a constrain plan must put a grammar on the wire to the local runtime")
	}
}

// TestLocalClientLeanWhenPlanTrusts proves the converse: a plan that trusts the model leaves
// decoding unconstrained, so a strong model is not slowed by a grammar it does not need.
func TestLocalClientLeanWhenPlanTrusts(t *testing.T) {
	if g, ok := sentGrammar(t, harness.Plan{}); ok {
		t.Fatalf("the zero plan must not constrain decoding, got grammar %q", g)
	}
}
