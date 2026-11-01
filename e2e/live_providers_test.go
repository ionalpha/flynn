package e2e

import (
	"os"
	"strings"
	"testing"
)

// providerTask is the one small job every provider is given: it needs a tool call and its
// result before the model can finish, so a provider that cannot carry a tool call through
// its wire format fails here instead of passing on a plain reply.
const providerTask = "Create the file provider.txt in the current directory containing the word ok, " +
	"using the write tool, then reply with one short sentence."

// assertToolRun checks what every provider must have done: called a tool that wrote the
// file, and converged. The file is the host-observed proof; the record is the claim.
func assertToolRun(t *testing.T, in *instance, res result) {
	t.Helper()
	requireExit(t, res, 0, "provider goal")
	rec := in.sealedRun(res)
	if len(rec.calls("write", "edit", "bash")) == 0 {
		t.Errorf("the model called no tool that could create the file; tools called: %v", callNames(rec))
	}
	if !rec.converged() {
		t.Errorf("the run did not converge\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
	}
	if _, err := in.workfile("provider.txt"); err != nil {
		t.Errorf("the file the tool call should have written is missing: %v", err)
	}
}

// TestLiveEachHostedProviderCarriesAToolCall runs the same tool-using goal against every
// hosted provider whose key is in the environment. A provider without a key is a skipped
// subtest named for it, so a run reports which providers it did not cover.
func TestLiveEachHostedProviderCarriesAToolCall(t *testing.T) {
	requireLive(t)
	for _, p := range hostedProviders {
		t.Run(p.name, func(t *testing.T) {
			if !p.hasKey() {
				t.Skipf("%s is not set, so the %s provider is not covered", p.keyEnv, p.name)
			}
			in := liveInstance(t, p.spec())
			assertToolRun(t, in, in.liveGoal(providerTask))
		})
	}
}

// TestLiveLocalModelCarriesAToolCall runs the goal against the local model that
// FLYNN_E2E_LOCAL_MODEL names, as provider:model (for example ollama:qwen3:8b, or
// llamacpp:<model> with LLAMACPP_BASE_URL pointing at a running llama-server). The runtime
// is the operator's to start; the lane does not download or launch one.
//
// A small local model may decline to use the tool. The run must still answer and its sealed
// record must verify; when the model made no tool call the test skips and says so, rather
// than passing as if the tool-call path had been carried.
func TestLiveLocalModelCarriesAToolCall(t *testing.T) {
	requireLive(t)
	spec := os.Getenv("FLYNN_E2E_LOCAL_MODEL")
	if spec == "" {
		t.Skip("FLYNN_E2E_LOCAL_MODEL is not set (provider:model, for example ollama:qwen3:8b)")
	}
	provider, _, _ := strings.Cut(spec, ":")
	if localBaseEnv(provider) == "" {
		t.Fatalf("FLYNN_E2E_LOCAL_MODEL=%q: the provider must be ollama, llamacpp or lmstudio", spec)
	}
	in := liveInstance(t, spec)
	res := in.liveGoal(providerTask)
	requireExit(t, res, 0, "local-model goal")
	rec := in.sealedRun(res)
	if len(rec.calls()) == 0 {
		t.Skipf("the local model %s answered without calling a tool, so its tool-call path was not exercised", spec)
	}
	assertToolRun(t, in, res)
}
