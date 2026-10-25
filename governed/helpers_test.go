package governed

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// stubPrompter answers every approval request with one decision and records that it was
// asked, so a test can assert the run stopped to ask rather than infer it from the outcome.
type stubPrompter struct {
	decision mission.ApprovalDecision
	asked    chan mission.ApprovalRequest
}

func newStubPrompter(dec mission.ApprovalDecision) *stubPrompter {
	return &stubPrompter{decision: dec, asked: make(chan mission.ApprovalRequest, 8)}
}

func (p *stubPrompter) Prompt(_ context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error) {
	p.asked <- req
	return p.decision, nil
}

// memBackends is a complete in-memory backend set under the registry a host would use.
func memBackends(t *testing.T) (Backends, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg, err := Registry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return Backends{Resources: st.Resources(reg), Jobs: st.Jobs(), Log: st.Log()}, st
}

// fakeTool is a tool that does nothing, for tests about which names a run offers.
type fakeTool string

func namedTool(name string) mission.Tool { return fakeTool(name) }

func (f fakeTool) Def() llm.Tool { return llm.Tool{Name: string(f), Description: "test tool"} }

func (f fakeTool) Invoke(context.Context, json.RawMessage) (string, error) { return "ok", nil }

// bridge is a [ToolBridge] over a fixed list.
type bridge []mission.Tool

func bridgeOf(tools ...mission.Tool) bridge { return bridge(tools) }

func (b bridge) Tools() []mission.Tool { return b }

func toolNames(tools []mission.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Def().Name)
	}
	return names
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}
