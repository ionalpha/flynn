package e2e

import (
	"strings"
	"testing"
)

// TestCommandSurface exercises the reference command table on the shipped binary: each
// command runs and exits with the documented code, so a broken dispatch or a renamed
// subcommand is caught. It does not need a model: these are the surfaces a user reaches
// before (and around) a run.
func TestCommandSurface(t *testing.T) {
	in := newInstance(t)

	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantText string // substring expected somewhere in stdout+stderr
	}{
		{"version", []string{"--version"}, 0, stampedVersion},
		{"runs-empty", []string{"runs"}, 0, "no runs yet"},
		{"models", []string{"models"}, 0, "catalog"},
		{"ps", []string{"ps"}, 0, "STATE"},
		{"auth-usage", []string{"auth"}, 1, "usage: flynn auth"},
		{"spine-usage", []string{"spine"}, 1, "usage: flynn spine"},
		{"spine-verify-missing", []string{"spine", "verify", "does-not-exist"}, 1, ""},
		{"serve-nothing", []string{"serve"}, 1, "nothing to do"},
		{"mcp-usage", []string{"mcp"}, 1, "usage: flynn mcp"},
		{"deploy-usage", []string{"deploy"}, 1, "usage: flynn deploy"},
		{"unknown-run", []string{"inspect", "no-such-run"}, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := in.run(c.args...)
			requireExit(t, res, c.wantExit, "flynn "+strings.Join(c.args, " "))
			if c.wantText != "" {
				requireContains(t, res.combined(), c.wantText, "flynn "+strings.Join(c.args, " "))
			}
		})
	}
}

// TestRunsAndInspectAfterGoal checks the post-run inspection surfaces: after a goal, the
// run is listed with its objective and can be inspected by id, replaying its recorded
// events. This is the "find a past run and audit it" path.
func TestRunsAndInspectAfterGoal(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("inspected answer"))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "a memorable objective string")
	requireExit(t, res, 0, "goal")
	runID := in.runID(res)

	runs := in.run("runs")
	requireContains(t, runs.stdout, "a memorable objective string", "runs objective")
	requireContains(t, runs.stdout, runID, "runs id")

	insp := in.run("inspect", runID)
	requireExit(t, insp, 0, "inspect")
	// The inspected run replays its events; the model's final text is part of the record.
	requireContains(t, insp.combined(), "inspected answer", "inspect replay")
}
