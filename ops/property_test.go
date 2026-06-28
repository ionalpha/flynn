package ops

import (
	"context"
	"encoding/json"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/integrations"
	"github.com/ionalpha/flynn/service"
)

// TestPropDeployContract asserts the hosting contract across any set of declared
// operations: an ops surface loads if and only if it declares a deploy operation, and
// when it loads, every operation becomes a callable tool and the deploy tool resolves.
func TestPropDeployContract(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		names := rapid.SliceOfDistinct(
			rapid.SampledFrom([]string{OpDeploy, OpStatus, OpLogs, OpList, OpTeardown, OpProvision}),
			func(s string) string { return s },
		).Draw(rt, "ops")

		operations := make([]integrations.Operation, 0, len(names))
		for _, n := range names {
			operations = append(operations, integrations.Operation{Name: n, Flow: json.RawMessage(returnFlow)})
		}
		spec := Spec{Targets: []service.Target{service.TargetStaticSite}, Operations: operations}
		block, err := json.Marshal(spec)
		if err != nil {
			rt.Fatalf("marshal: %v", err)
		}
		m := extension.Mount{
			ID:      "x",
			Name:    "x",
			Spec:    extension.Spec{Auth: extension.AuthSpec{Type: "none"}},
			Surface: extension.SurfaceOps,
			Block:   block,
		}

		hasDeploy := false
		for _, n := range names {
			if n == OpDeploy {
				hasDeploy = true
			}
		}

		h := NewHandler()
		err = h.OnLoad(context.Background(), m)

		if !hasDeploy {
			if err == nil {
				rt.Fatalf("expected load to fail without a deploy operation (ops=%v)", names)
			}
			return
		}
		if err != nil {
			rt.Fatalf("load failed with deploy present (ops=%v): %v", names, err)
		}
		if got := len(h.Tools("x")); got != len(names) {
			rt.Fatalf("expected %d tools, got %d (ops=%v)", len(names), got, names)
		}
		if h.DeployTool("x") == nil {
			rt.Fatalf("deploy tool did not resolve (ops=%v)", names)
		}
	})
}
