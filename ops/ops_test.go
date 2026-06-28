package ops

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/integrations"
	"github.com/ionalpha/flynn/service"
)

// returnFlow is a deploy flow with no network step: it returns a literal result, so
// the handler's contract and mounting can be tested without a transport.
const returnFlow = `{"steps":[{"op":"return","return":{"value":{"url":"https://site.example","id":"dep-1"}}}]}`

func opsMount(t *testing.T, spec Spec) extension.Mount {
	t.Helper()
	block, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	return extension.Mount{
		ID:      "cf",
		Name:    "cloudflare",
		Spec:    extension.Spec{Provider: "cloudflare", BaseURL: "https://api.example", Auth: extension.AuthSpec{Type: "none"}},
		Surface: extension.SurfaceOps,
		Block:   block,
	}
}

// TestOpsMountsHostingOperations proves a valid ops surface mounts its operations as
// tools, records its targets, and resolves the deploy tool by name.
func TestOpsMountsHostingOperations(t *testing.T) {
	h := NewHandler()
	spec := Spec{
		Targets: []service.Target{service.TargetStaticSite},
		Operations: []integrations.Operation{
			{Name: OpDeploy, Flow: json.RawMessage(returnFlow)},
			{Name: OpStatus, Flow: json.RawMessage(returnFlow)},
		},
	}
	if err := h.OnLoad(context.Background(), opsMount(t, spec)); err != nil {
		t.Fatalf("OnLoad: %v", err)
	}

	tools := h.Tools("cf")
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if h.DeployTool("cf") == nil {
		t.Fatal("deploy tool not resolved")
	}
	if got := h.Targets("cf"); len(got) != 1 || got[0] != service.TargetStaticSite {
		t.Fatalf("targets: %v", got)
	}

	// The deploy flow returns its literal result, exercising the delegated engine.
	out, err := h.DeployTool("cf").Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke deploy: %v", err)
	}
	if out == "" {
		t.Fatal("deploy returned no result")
	}

	if err := h.OnUnload(context.Background(), "cf"); err != nil {
		t.Fatalf("OnUnload: %v", err)
	}
	if len(h.Tools("cf")) != 0 || len(h.Targets("cf")) != 0 {
		t.Fatal("unload left state behind")
	}
}

// TestOpsRequiresDeploy proves a hosting provider that declares no deploy operation
// is rejected at load, not at first use.
func TestOpsRequiresDeploy(t *testing.T) {
	h := NewHandler()
	spec := Spec{Operations: []integrations.Operation{{Name: OpStatus, Flow: json.RawMessage(returnFlow)}}}
	if err := h.OnLoad(context.Background(), opsMount(t, spec)); err == nil {
		t.Fatal("expected a provider with no deploy operation to be rejected")
	}
}

// TestOpsRejectsUnknownTarget proves an ops surface declaring a target the system does
// not understand fails closed at load.
func TestOpsRejectsUnknownTarget(t *testing.T) {
	h := NewHandler()
	spec := Spec{
		Targets:    []service.Target{"mainframe"},
		Operations: []integrations.Operation{{Name: OpDeploy, Flow: json.RawMessage(returnFlow)}},
	}
	if err := h.OnLoad(context.Background(), opsMount(t, spec)); err == nil {
		t.Fatal("expected an unknown target to be rejected")
	}
}
