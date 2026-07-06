package e2e

import (
	"os"
	"testing"
)

// TestRealModelSmoke is the single opt-in lane that hits a real hosted model instead of
// the scripted server. It is skipped unless FLYNN_E2E_REAL is set, so the default suite
// stays offline, free, and deterministic. It runs a benign happy-path only (a plain
// reply, no tools, no adversarial content), so it never sends anything that could trip a
// provider's abuse detection; every adversarial scenario stays on the scripted lane.
//
// Configure the model with FLYNN_E2E_REAL_MODEL (default anthropic:claude-haiku-4-5);
// the matching provider key is read from the ambient environment.
func TestRealModelSmoke(t *testing.T) {
	if os.Getenv("FLYNN_E2E_REAL") == "" {
		t.Skip("set FLYNN_E2E_REAL=1 to run the opt-in real-model lane")
	}
	model := os.Getenv("FLYNN_E2E_REAL_MODEL")
	if model == "" {
		model = "anthropic:claude-haiku-4-5-20251001"
	}

	in := newInstance(t)
	in.model = model
	// Pass the provider key through from the ambient environment (scrubbedEnv removed it
	// for the offline lanes); no base URL override, so the real endpoint is used.
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "DEEPSEEK_API_KEY"} {
		if v := os.Getenv(k); v != "" {
			in.setEnv(k, v)
		}
	}

	res := in.run("-no-learn", "goal", "Reply with a single short sentence confirming you are working, then stop. Do not use any tools.")
	requireExit(t, res, 0, "real-model goal")
	requireExit(t, in.verify(in.runID(res)), 0, "verify real-model run")
}
