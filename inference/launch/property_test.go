package launch

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestBuildPlanLoopbackInvariantProperty asserts the safety invariants hold for every
// valid plan: whatever the port and trusted template, the server is bound to loopback
// and only to loopback, and the forced chat template appears in the command. No valid
// configuration can produce a plan that listens off the machine or omits the contract.
func TestBuildPlanLoopbackInvariantProperty(t *testing.T) {
	templates := make([]string, 0, len(knownChatTemplates))
	for name := range knownChatTemplates {
		templates = append(templates, name)
	}
	rapid.Check(t, func(rt *rapid.T) {
		port := rapid.IntRange(1, 65535).Draw(rt, "port")
		tmpl := rapid.SampledFrom(templates).Draw(rt, "tmpl")
		ctx := rapid.IntRange(0, 1_000_000).Draw(rt, "ctx")

		plan, err := BuildPlan(Config{BinPath: "b", WeightsPath: "w", Model: localModel(tmpl), Port: port, CtxSize: ctx})
		if err != nil {
			rt.Fatalf("a valid config was refused: %v", err)
		}
		if plan.Host != "127.0.0.1" {
			rt.Fatalf("plan host is not loopback: %q", plan.Host)
		}
		if plan.BaseURL != fmt.Sprintf("http://127.0.0.1:%d/v1", port) {
			rt.Fatalf("base url not loopback: %q", plan.BaseURL)
		}
		// The only --host value may be loopback.
		for i := range len(plan.Argv) - 1 {
			if plan.Argv[i] == "--host" && plan.Argv[i+1] != "127.0.0.1" {
				rt.Fatalf("argv binds a non-loopback host: %q", plan.Argv[i+1])
			}
		}
		if !argvHasFlag(plan.Argv, "--chat-template", tmpl) {
			rt.Fatalf("argv missing the forced template %q: %v", tmpl, plan.Argv)
		}
		if strings.Contains(strings.Join(plan.Argv, " "), "0.0.0.0") {
			rt.Fatalf("argv must never bind the wildcard address: %v", plan.Argv)
		}
	})
}
