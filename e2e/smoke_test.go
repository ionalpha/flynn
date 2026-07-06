package e2e

import (
	"strings"
	"testing"
)

// TestVersionStamped asserts the binary reports exactly the version linked into it, so
// a release build that fails to stamp internal/version.Version (and would instead
// report the source default) is caught before tag.
func TestVersionStamped(t *testing.T) {
	in := newInstance(t)
	res := in.run("--version")
	requireExit(t, res, 0, "flynn --version")
	if got := strings.TrimSpace(res.stdout); got != stampedVersion {
		t.Fatalf("version: expected %q, got %q", stampedVersion, got)
	}
}

// TestHappyPathConverges drives a one-turn goal against a scripted model and asserts the
// full observable happy path: the run converges (exit 0), prints a run id, lists the run
// as converged, and leaves a record that `spine verify` accepts on every tier.
func TestHappyPathConverges(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("The answer is 42."))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "state the answer")
	requireExit(t, res, 0, "goal")
	requireContains(t, res.stdout, "The answer is 42.", "goal output")

	runID := in.runID(res)

	runs := in.run("runs")
	requireExit(t, runs, 0, "runs")
	requireContains(t, runs.stdout, runID, "runs table")

	ver := in.verify(runID)
	requireExit(t, ver, 0, "spine verify")
	requireContains(t, ver.stdout, "integrity:", "verify report")
	requireContains(t, ver.stdout, "VERIFIED", "verify integrity")
	requireContains(t, ver.stdout, "governance:", "verify report")
}

// TestToolSurface records what the binary advertises to the model on the first turn and
// logs it. It is a discovery/inventory test (always passes): the advertised tool set is
// the capability surface later scenarios script calls against, and logging it here keeps
// the suite honest about what the shipped default actually exposes.
func TestToolSurface(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("done"))
	in := newInstance(t).withModel(fake)
	res := in.run("-no-learn", "goal", "do nothing")
	requireExit(t, res, 0, "goal")

	req := fake.request(t, 0)
	t.Logf("model=%s  tools=%d", req.Model, len(req.Tools))
	t.Logf("advertised tools: %s", strings.Join(req.Tools, ", "))
	t.Logf("system prompt (first 800 bytes):\n%s", head(req.System, 800))
	if len(req.Tools) == 0 {
		t.Logf("NOTE: no tools advertised on the first turn (goal may add them after planning)")
	}
}

func head(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
