package mission

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestHostNoteNamesTheShellForEachPlatform: a model told only to write "a shell command"
// writes the one it was trained on. On Windows that is POSIX, and the check can never pass,
// so the note has to name the shell that reads the command and what it lacks.
func TestHostNoteNamesTheShellForEachPlatform(t *testing.T) {
	cases := []struct {
		goos string
		want []string
	}{
		{"windows", []string{"Windows", "cmd.exe", "test", "$(...)"}},
		{"darwin", []string{"macOS", "sh -c", "BSD"}},
		{"linux", []string{"linux", "sh -c", "POSIX"}},
	}
	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			note := hostNote(c.goos)
			for _, w := range c.want {
				if !strings.Contains(note, w) {
					t.Errorf("hostNote(%q) = %q, missing %q", c.goos, note, w)
				}
			}
		})
	}
	if HostNote() != hostNote(runtime.GOOS) {
		t.Fatal("HostNote does not describe the host it runs on")
	}
}

// TestPlannerIsToldTheHost: the planner is the one that writes each item's check, so the
// note has to reach its system prompt or it will keep planning checks for the wrong shell.
func TestPlannerIsToldTheHost(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText(`[]`))
	p := NewPlanner(model)
	if _, err := p.Plan(context.Background(), goalResource(t, goal.Spec{Objective: "o", StopCondition: "c"})); err != nil {
		t.Fatal(err)
	}
	if sys := model.Requests()[0].System; !strings.Contains(sys, HostNote()) {
		t.Fatalf("planner system prompt does not carry the host note: %q", sys)
	}
}
