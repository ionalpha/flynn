package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/state"
)

func TestMergeSkillsKeepsFlagsFirstAndEachOnce(t *testing.T) {
	got := mergeSkills([]string{"b", " a ", "b"}, []string{"a", "", "c"})
	if want := "b,a,c"; strings.Join(got, ",") != want {
		t.Fatalf("mergeSkills = %v, want %s", got, want)
	}
	if got := mergeSkills(nil, nil); len(got) != 0 {
		t.Fatalf("mergeSkills(nil, nil) = %v, want none", got)
	}
}

func TestGoalSpecFileCarriesSkills(t *testing.T) {
	spec, err := loadGoalSpecFile(writeGoalSpec(t, `{"objective": "x", "skills": ["feedback-loop"]}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(spec.Skills) != 1 || spec.Skills[0] != "feedback-loop" {
		t.Fatalf("skills = %v", spec.Skills)
	}
}

// Skills load into one run's first turn, so only `flynn goal` can honour --skill. A
// command that ignored it would leave the operator believing a skill was in force.
func TestSkillFlagIsRefusedByACommandThatCannotHonourIt(t *testing.T) {
	for _, cmd := range []string{"runs", "resume", ""} {
		t.Run("flynn "+cmd, func(t *testing.T) {
			var errOut strings.Builder
			code := routeCommand(cmd, []string{cmd}, invocation{
				stdout:  &strings.Builder{},
				stderr:  &errOut,
				dataDir: t.TempDir(),
				skills:  []string{"feedback-loop"},
			})
			if code != 2 || !strings.Contains(errOut.String(), "--skill") {
				t.Fatalf("code = %d, stderr = %q, want a usage refusal naming --skill", code, errOut.String())
			}
		})
	}
}

// lastUserText is the text of the newest user message in req.
func lastUserText(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != llm.RoleUser {
			continue
		}
		var sb strings.Builder
		for _, b := range req.Messages[i].Blocks {
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	return ""
}

// A skill named for the run reaches the model in its first turn, a skill whose declared
// trigger fires reaches the next turn, and the run's record names each route.
func TestStartAndTriggerLoadsReachTheModelAndTheRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := memStore(t)
	skills := store.Skills()
	seed := func(slug, body string, ts ...state.SkillTrigger) {
		t.Helper()
		if _, err := skills.Upsert(ctx, state.Skill{Slug: slug, Name: slug, Description: "Use for " + slug, Body: body, Scope: state.BundledScope, Triggers: ts}); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	seed("named-skill", "NAMED-BODY")
	seed("watcher", "TRIGGERED-BODY", state.SkillTrigger{Kind: "tool", Match: "glob"})
	set := skilltool.New(skills, skilltool.WithStart("named-skill", "no-such-skill"))

	model := llmtest.NewScripted(
		llmtest.CallTool("c1", "glob", json.RawMessage(`{"pattern":"*.nothing"}`)),
		llmtest.SayText("done"),
	)
	var out bytes.Buffer
	_, runID, _, _ := driveRequest{
		out:       &out,
		model:     model,
		plan:      harness.Plan{},
		workdir:   t.TempDir(),
		objective: "look around",
		system:    defaultSystemPrompt,
		rstore:    store.Resources(mustRegistry(t)),
		jq:        store.Jobs(),
		log:       store.Log(),
	}.withOptions(withSkills(set), withOfferedSkills([]string{"watcher"})).execute(ctx)

	reqs := model.Requests()
	if len(reqs) < 2 {
		t.Fatalf("model saw %d requests, want 2", len(reqs))
	}
	if first := lastUserText(reqs[0]); !strings.Contains(first, "NAMED-BODY") || strings.Contains(first, "TRIGGERED-BODY") {
		t.Fatalf("first turn = %q, want the named skill only", first)
	}
	if next := lastUserText(reqs[1]); !strings.Contains(next, "TRIGGERED-BODY") {
		t.Fatalf("turn after the glob call = %q, want the triggered body", next)
	}
	// The tool result must stay directly after its call: the brief rides in the same user
	// message, as a block after the result.
	msgs := reqs[1].Messages
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser || last.Blocks[0].Kind != llm.KindToolResult {
		t.Fatalf("last message = %+v, want the tool result first", last)
	}

	events, err := session.History(ctx, store.Log(), runID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var got []string
	for _, ev := range events {
		if ev.Kind == session.KindSkill {
			got = append(got, ev.Via+":"+ev.Skill)
		}
	}
	want := "offered:watcher,start:named-skill,skipped:no-such-skill,trigger:watcher"
	if strings.Join(got, ",") != want {
		t.Fatalf("skill events = %v, want %s", got, want)
	}
	if loaded := set.Loaded(); len(loaded) != 2 {
		t.Fatalf("Loaded() = %v, want both bodies", loaded)
	}
	if len(set.Reads()) != 0 {
		t.Fatalf("Reads() = %v: a host load is not a model read", set.Reads())
	}
}

func TestSkillLineNamesWhatTheHostDidAndNotWhatTheModelDid(t *testing.T) {
	for via, want := range map[string]string{
		"start":      "skill: loaded s at start",
		"trigger":    "skill: loaded s (why)",
		"skipped":    "skill: skipped s (why)",
		"suppressed": "skill: kept s on offer only (why)",
		"offered":    "",
		"read":       "",
	} {
		if got := skillLine(session.Event{Kind: session.KindSkill, Skill: "s", Via: via, Reason: "why"}); got != want {
			t.Errorf("skillLine(%s) = %q, want %q", via, got, want)
		}
	}
}
