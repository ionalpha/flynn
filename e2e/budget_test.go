package e2e

import (
	"strings"
	"testing"
)

// TestBudgetTokenCeilingHalts drives a model that never converges (it keeps asking to
// run a command) under a small -max-tokens ceiling. The run must stop once the ceiling
// is crossed, with a distinct non-zero exit and a budget_exceeded reason, and the state
// must survive: the halted run is still listed and inspectable, and the metered spend is
// bounded near the ceiling rather than running away.
func TestBudgetTokenCeilingHalts(t *testing.T) {
	fake := newFakeOpenAIFunc(t, func(_ oaiRequest, _ int) oaiReply {
		return toolCall("c", "bash", `{"command":"echo hi"}`)
	})
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "-max-tokens", "20", "goal", "loop until the budget stops me")
	requireExit(t, res, 1, "budget-exhausted goal")
	requireContains(t, res.combined(), "budget_exceeded", "budget stop reason")
	requireContains(t, res.combined(), "stalled", "graceful stall")

	// The ceiling actually bounded the run: a runaway would have made many calls. With
	// 15 metered tokens per call and a 20-token ceiling, the run stops within a couple
	// of calls, not dozens.
	if n := fake.count(); n > 4 {
		t.Fatalf("budget did not bound the run: %d model calls made under a 20-token ceiling", n)
	}

	// State intact: the partial run persisted and is inspectable by id.
	runID := in.runID(res)
	runs := in.run("runs")
	requireExit(t, runs, 0, "runs after budget stop")
	requireContains(t, runs.stdout, runID, "halted run still listed")

	insp := in.run("inspect", runID)
	requireExit(t, insp, 0, "inspect halted run")
}

// TestBudgetZeroIsUnlimited confirms the documented default: a zero ceiling (the flag's
// default) does not stop a converging run. This guards against a flipped comparison that
// would refuse every run.
func TestBudgetZeroIsUnlimited(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("converged fine"))
	in := newInstance(t).withModel(fake)
	res := in.run("-no-learn", "-max-tokens", "0", "goal", "just finish")
	requireExit(t, res, 0, "zero ceiling is unlimited")
	if strings.Contains(res.combined(), "budget_exceeded") {
		t.Fatalf("zero ceiling wrongly enforced a budget:\n%s", res.combined())
	}
}
