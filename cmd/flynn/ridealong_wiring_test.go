package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/learn"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// theLesson is what run one is scripted to learn. Distinct enough that finding it
// in run two's transcript can only mean it travelled.
const theLesson = "generated files are exempt from the reviewer pass"

// seedSkill puts one readable skill in the store and returns its id, which is what
// the anchor is written against.
func seedSkill(t *testing.T, store *sqlite.Store) string {
	t.Helper()
	sk, err := store.Skills().Upsert(context.Background(), state.Skill{
		Slug:        "tidy-diff",
		Name:        "tidy-diff",
		Description: "Reduce a change to the smallest diff that still does the job.",
		Body:        "Read the diff as a reviewer would, then remove what the change does not need.",
	})
	if err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	return sk.ID
}

// readsTheSkill scripts a model that loads the skill and then converges.
func readsTheSkill() *llmtest.ScriptedModel {
	return llmtest.NewScripted(
		llmtest.CallTool("c1", "skill_read", json.RawMessage(`{"skill":"tidy-diff"}`)),
		llmtest.SayText("done"),
	)
}

// TestRideAlongClosesTheLoopAcrossTwoRuns is the standalone proof for the pull half
// of memory: with no host present, a lesson one run learned while working from a
// procedure arrives on the next run's read of that procedure.
//
// Both halves are Flynn's own, which is the whole reason this pairing was chosen. It
// writes the anchor (the skills the run loaded, which it already tracks for
// reinforcement) and it makes the read that matches on it (skill_read, its own tool).
// Neither end needs a host to name a referent or to hand over a cue.
func TestRideAlongClosesTheLoopAcrossTwoRuns(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	skillID := seedSkill(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer

	// Run one reads the skill and learns something. The distiller is scripted, so what
	// is captured is fixed and the anchoring is the only thing under test.
	learner := &fakeDistiller{lessons: []learn.Lesson{{Kind: learn.LessonMemory, Body: theLesson}}}
	if _, err := runLearningMission(ctx, &out, readsTheSkill(), harness.Plan{}, learner, dir, "tidy the diff", "", store, nil, false, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The lesson is anchored to the skill the run loaded. Without this the read half
	// finds nothing however well it is wired, which is why it is asserted separately
	// from the surfacing below.
	anchored, err := store.Memory().Recall(ctx, state.RecallQuery{Anchors: []state.Anchor{state.SkillAnchor(skillID)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(anchored) != 1 || anchored[0].Content != theLesson {
		t.Fatalf("memory anchored to the skill = %+v, want the lesson from run one", anchored)
	}

	// Run two loads the same skill and is handed what run one learned, on the tool
	// result it asked for the procedure with. No distiller: this run learns nothing,
	// it only reads.
	second := readsTheSkill()
	if _, err := runLearningMission(ctx, &out, second, harness.Plan{}, nil, dir, "tidy the diff again", "", store, nil, false, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !inAToolResult(second.Requests()) {
		t.Fatal("run two's skill_read did not carry what run one learned from the same skill")
	}
	// It arrived on the read and only on the read. The wake digest is the other way a
	// memory reaches a reader unasked, and a test that only asked "is the text in here
	// somewhere" would pass on the digest alone and prove nothing about the ride-along.
	// An agent-sourced lesson is not digest-pushable, so the prompt carrying it would
	// itself be a finding.
	if inTheStandingInstructions(second.Requests()) {
		t.Error("the lesson was also pushed into the standing instructions; the surfacing is not what delivered it")
	}

	// And the pull side counted the delivery, which is what stops last-used-at
	// degrading into a record of what the digest pushed.
	usage, err := store.Memory().Usage(ctx, []string{anchored[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].UseCount() == 0 {
		t.Fatalf("usage = %+v, want the surfacing counted as a use", usage)
	}
}

// inAToolResult reports whether the lesson came back on a tool result in any of
// these requests, which is where a ride-along lands.
func inAToolResult(reqs []llm.Request) bool {
	for _, r := range reqs {
		for _, m := range r.Messages {
			for _, b := range m.Blocks {
				if b.ToolResult != nil && strings.Contains(b.ToolResult.Content, theLesson) {
					return true
				}
			}
		}
	}
	return false
}

// inTheStandingInstructions reports whether the lesson was in the prompt, which is
// where the wake digest and the objective recall put what they select.
func inTheStandingInstructions(reqs []llm.Request) bool {
	for _, r := range reqs {
		if strings.Contains(r.System, theLesson) {
			return true
		}
	}
	return false
}
