package e2e

import (
	"strings"
	"testing"
)

// TestLiveSecondRunRecallsWhatTheFirstLeft runs two goals on one install with the learning
// loop on. The first reads a bundled skill and does a small task whose convention is worth
// keeping; the second is handed what the first left.
//
// What the distiller keeps varies with the model, so the first run is only required to
// leave something (a memory awaiting review, or a distilled skill); a run that leaves
// nothing skips the recall half with that stated. What the model did is read from the
// record and the store: the skill read is credited to the skill, the promoted memory is
// pushed at the second run, and a distilled skill is read by name in it.
func TestLiveSecondRunRecallsWhatTheFirstLeft(t *testing.T) {
	in := liveInstance(t, liveModel(t))

	first := in.liveGoalLearning(
		"Read the bundled skill systematic-debugging with the skill_read tool. Then create the file " +
			"greeting-fr.txt containing the single line bonjour. This project's convention, which later work " +
			"on it should follow: every greeting file is named greeting-<language code>.txt and holds exactly " +
			"one lowercase word and a trailing newline. Do not use the bash tool.",
	)
	requireExit(t, first, 0, "first goal with learning on")
	rec := in.sealedRun(first)

	var read bool
	for _, c := range rec.calls("skill_read") {
		if c.input["skill"] == "systematic-debugging" {
			read = true
		}
	}
	if !read {
		t.Fatalf("the model never read the bundled skill; tools called: %v", callNames(rec))
	}
	ls := in.run("skill", "ls")
	requireExit(t, ls, 0, "skill ls")
	f := strings.Fields(skillRow(ls.stdout, "systematic-debugging"))
	if len(f) < 3 || f[2] == "0" {
		t.Errorf("the skill read was not credited to the skill:\n%s", ls.stdout)
	}

	pending := in.run("memory", "ls", "--pending")
	requireExit(t, pending, 0, "memory ls --pending")
	var memoryID string
	for _, line := range strings.Split(pending.stdout, "\n") {
		if strings.Contains(line, "(awaiting review)") {
			memoryID = strings.Fields(line)[0]
		}
	}
	var localSkill string
	for _, line := range strings.Split(ls.stdout, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.Contains(line, "local") {
			localSkill = f[0]
		}
	}
	if memoryID == "" && localSkill == "" {
		t.Skip("the distiller kept no memory and no skill from the first run, so there is nothing for the second to recall")
	}

	if memoryID != "" {
		requireExit(t, in.run("memory", "promote", memoryID), 0, "memory promote")
	}
	objective := "Create the file greeting-de.txt for German."
	if localSkill != "" {
		objective += " First read the skill " + localSkill + " with the skill_read tool."
	}
	second := in.liveGoalLearning(objective + " Do not use the bash tool.")
	requireExit(t, second, 0, "second goal")
	secondRec := in.sealedRun(second)

	if memoryID != "" {
		usage := in.run("memory", "usage")
		requireExit(t, usage, 0, "memory usage")
		if !strings.Contains(usage.stdout, "pushed 1") {
			t.Errorf("the promoted memory was not pushed at the second run:\n%s", usage.stdout)
		}
	}
	if localSkill != "" {
		var read bool
		for _, c := range secondRec.calls("skill_read") {
			if c.input["skill"] == localSkill {
				read = true
			}
		}
		if !read {
			t.Errorf("the second run did not read the skill %q the first left; tools called: %v", localSkill, callNames(secondRec))
		}
	}
}
