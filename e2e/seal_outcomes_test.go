package e2e

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A run that does not converge is the one somebody will want to check, so its record has
// to exist and verify like any other. These tests stop a run three ways (a token ceiling,
// `flynn kill`, a refused provider call) and then read the record back through the
// shipped binary: `spine verify` reports the integrity tier, `spine export` writes the
// record, and both say how the run ended rather than leaving it looking like a success.

// requireSealedWithOutcome checks that the run's record verifies and exports, and that the
// verify report names how the run ended.
func requireSealedWithOutcome(t *testing.T, in *instance, runID, outcome string) {
	t.Helper()
	ver := in.verify(runID)
	requireExit(t, ver, 0, "spine verify of a stopped run")
	requireContains(t, ver.stdout, "integrity:    VERIFIED", "the stopped run's record verifies")
	requireContains(t, ver.stdout, "outcome:", "verify names the outcome")
	requireContains(t, ver.stdout, outcome, "verify names how the run ended")
	if strings.Contains(ver.stdout, "GROUNDED (") {
		t.Fatalf("a stopped run's record reads as a grounded success:\n%s", ver.stdout)
	}

	path, exp := in.export(runID)
	requireExit(t, exp, 0, "spine export of a stopped run")
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Fatalf("spine export wrote no record at %s: %v", path, err)
	}
}

// TestSealedRecordWhenTheTokenCeilingStopsTheRun: a model that never converges is stopped
// by -max-tokens, and the run is still sealed.
func TestSealedRecordWhenTheTokenCeilingStopsTheRun(t *testing.T) {
	fake := newFakeOpenAIFunc(t, func(oaiRequest, int) oaiReply {
		return toolCall("c", "bash", `{"command":"echo hi"}`)
	})
	in := newInstance(t).withModel(fake)

	res := in.run("-max-tokens", "20", "goal", "loop until the budget stops me")
	requireExit(t, res, 1, "budget-exhausted goal keeps its exit code")
	requireContains(t, res.combined(), "budget_exceeded", "budget stop reason")

	requireSealedWithOutcome(t, in, in.runID(res), "budget_exceeded")
}

// TestSealedRecordWhenTheOperatorKillsTheRun: `flynn kill` from a second invocation stops
// a goal mid-turn, and the killed run is still sealed with the kill as its outcome.
func TestSealedRecordWhenTheOperatorKillsTheRun(t *testing.T) {
	release := make(chan struct{})
	var answered atomic.Bool
	fake := newFakeOpenAIFunc(t, func(oaiRequest, int) oaiReply {
		if answered.CompareAndSwap(false, true) {
			<-release
			return toolCall("call_1", "write", `{"path":"after-kill.txt","content":"x"}`)
		}
		return finalText("Done.")
	})
	in := newInstance(t).withModel(fake)

	const objective = "write a file after a long think, then be sealed"
	goal := in.start("-no-learn", "goal", objective)
	fake.waitForCount(t, 1, 30*time.Second)
	id := waitForRun(t, in, objective)

	requireExit(t, in.run("kill", id, "wrong repository"), 0, "kill")
	waitForPhase(t, in, id, "Stalled")
	close(release)

	select {
	case err := <-goal.done:
		if err == nil {
			t.Fatal("the killed goal exited 0")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the killed goal did not exit")
	}
	requireContains(t, goal.errb.String(), "stopped by the operator: wrong repository", "the killed goal's exit")

	requireSealedWithOutcome(t, in, id, "stopped by the operator: wrong repository")
}

// TestSealedRecordWhenTheProviderRefusesTheRun: a provider that rejects the credential
// fails the run before it converges, and the failure is sealed like any other ending.
func TestSealedRecordWhenTheProviderRefusesTheRun(t *testing.T) {
	fake := newFakeOpenAI(t, oaiReply{Status: 401, ErrType: "invalid_request_error", ErrCode: "invalid_api_key", ErrMsg: "Incorrect API key provided"})
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "run against a provider that refuses")
	if res.code == 0 {
		t.Fatalf("a run the provider refused exited 0:\n%s", res.combined())
	}

	requireSealedWithOutcome(t, in, in.runID(res), "stalled")
}
