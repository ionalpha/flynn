package e2e

import (
	"strings"
	"testing"
)

// TestColdStartZeroConfig proves the cold-start posture: a brand-new data dir with no
// config beyond a model key runs a goal to completion AND is safe by default in the same
// fresh state. "Safe" is not asserted by inspecting config but by behavior: on the same
// untouched install, a wildcard bind is still refused and a path-jail escape is still
// denied. A default that had to be hardened by hand would fail one of these.
func TestColdStartZeroConfig(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("cold start works"))
	in := newInstance(t).withModel(fake)

	// Nothing has run yet in this fresh data dir.
	runsBefore := in.run("runs")
	requireExit(t, runsBefore, 0, "runs on a fresh install")
	requireContains(t, runsBefore.stdout, "no runs yet", "fresh install has no runs")

	// It runs with only a key and default config.
	res := in.run("-no-learn", "goal", "a first goal on a fresh machine")
	requireExit(t, res, 0, "cold-start goal")
	requireContains(t, res.combined(), "cold start works", "fresh install converges")
	requireExit(t, in.verify(in.runID(res)), 0, "fresh run verifies")

	// Safe by default on the same untouched install: exposing to the world is refused...
	bind := in.run("serve", "--api-addr", "0.0.0.0:8080")
	requireExit(t, bind, 1, "wildcard bind refused on a fresh install")

	// ...and a sandbox escape is denied with no hardening applied. A traversal target is
	// actively denied by the jail on every OS (an absolute drive path is instead
	// neutralized by path-joining, a different mechanism), so it gives a clear signal.
	escapeFake := newFakeOpenAIQueue(
		t,
		toolCall("r", "read", `{"path":"../../../../../../etc/passwd"}`),
		finalText("could not read it"),
	)
	in2 := newInstance(t).withModel(escapeFake)
	in2.run("-no-learn", "goal", "read a host file on a fresh install")
	var denied bool
	for i := 1; i < escapeFake.count(); i++ {
		for _, m := range escapeFake.request(t, i).Messages {
			if m.Role == "tool" && (strings.Contains(m.Content, "sandbox_denied") || strings.Contains(m.Content, "denied by the sandbox")) {
				denied = true
			}
		}
	}
	if !denied {
		t.Fatal("a fresh install did not deny a host-file read by default")
	}
}
