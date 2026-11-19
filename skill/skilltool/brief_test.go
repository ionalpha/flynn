package skilltool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/skill/skillload"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/state"
)

func triggered(slug string, ts ...state.SkillTrigger) state.Skill {
	return state.Skill{
		Slug:        slug,
		Name:        slug,
		Description: "Use when " + slug + " applies.",
		Body:        "Procedure for " + slug + ".",
		Scope:       state.BundledScope,
		Triggers:    ts,
	}
}

var (
	repeat3  = state.SkillTrigger{Kind: "repeat", Count: 3}
	slow60   = state.SkillTrigger{Kind: "slow", Seconds: 60}
	callsTo  = func(tool string) state.SkillTrigger { return state.SkillTrigger{Kind: "tool", Match: tool} }
	runTests = mission.ObservedCall{Tool: "exec", Input: json.RawMessage(`{"command":"go test ./..."}`)}
)

// briefSet seeds skills and returns the set with the briefer its read tool exposes.
func briefSet(t *testing.T, seed []state.Skill, opts ...skilltool.Option) (*skilltool.Set, mission.Briefer, map[string]string) {
	t.Helper()
	skills := state.NewMemory().Skills()
	ids := map[string]string{}
	for _, sk := range seed {
		got, err := skills.Upsert(context.Background(), sk)
		if err != nil {
			t.Fatalf("seed %s: %v", sk.Slug, err)
		}
		ids[sk.Slug] = got.ID
	}
	set := skilltool.New(skills, opts...)
	for _, tool := range set.Tools() {
		if b, ok := tool.(mission.Briefer); ok {
			return set, b, ids
		}
	}
	t.Fatal("no tool in the set implements mission.Briefer")
	return nil, nil, nil
}

func brief(t *testing.T, b mission.Briefer, state json.RawMessage, calls ...mission.ObservedCall) mission.BriefOutput {
	t.Helper()
	out, err := b.Brief(context.Background(), mission.BriefInput{Calls: calls, State: state})
	if err != nil {
		t.Fatalf("Brief: %v", err)
	}
	return out
}

func vias(out mission.BriefOutput) []string {
	var got []string
	for _, n := range out.Notes {
		got = append(got, n.Via+":"+n.Name)
	}
	return got
}

func equal(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// TestRepeatedCommandLoadsFeedbackLoopOnce: the same command three times loads the body
// into the next turn, the run record says why, and a fourth run of it loads nothing more.
func TestRepeatedCommandLoadsFeedbackLoopOnce(t *testing.T) {
	set, b, ids := briefSet(t, []state.Skill{triggered("feedback-loop", repeat3)})

	out := brief(t, b, nil, runTests)
	if out.Text != "" || len(out.Notes) != 0 {
		t.Fatalf("one run loaded %q %v, want nothing", out.Text, out.Notes)
	}
	out = brief(t, b, out.State, runTests, runTests)
	if !strings.Contains(out.Text, "Procedure for feedback-loop.") {
		t.Fatalf("third run did not load the body, text = %q", out.Text)
	}
	if got := vias(out); !equal(got, []string{"trigger:feedback-loop"}) {
		t.Fatalf("notes = %v, want one trigger load", got)
	}
	if !strings.Contains(out.Notes[0].Reason, "3 times") {
		t.Fatalf("reason = %q, want it to say the command ran 3 times", out.Notes[0].Reason)
	}

	again := brief(t, b, out.State, runTests)
	if again.Text != "" || len(again.Notes) != 0 {
		t.Fatalf("fourth run loaded %q %v, want the body once per run", again.Text, again.Notes)
	}

	// A load nobody asked for is no evidence the skill helped.
	if got := set.Loaded(); len(got) != 1 || got[0] != ids["feedback-loop"] {
		t.Fatalf("Loaded() = %v, want the trigger-loaded skill", got)
	}
	if got := set.Reads(); len(got) != 0 {
		t.Fatalf("Reads() = %v, want none: a trigger load is not a model read", got)
	}
}

func TestSlowCheckLoadsTheSkill(t *testing.T) {
	_, b, _ := briefSet(t, []state.Skill{triggered("feedback-loop", slow60)})

	fast := runTests
	fast.Elapsed = 59 * time.Second
	if out := brief(t, b, nil, fast); out.Text != "" {
		t.Fatalf("a 59s check loaded %q, want nothing", out.Text)
	}
	slow := runTests
	slow.Elapsed = 61 * time.Second
	if out := brief(t, b, nil, slow); !strings.Contains(out.Text, "feedback-loop") {
		t.Fatalf("a 61s check did not load, text = %q", out.Text)
	}
}

// TestCapKeepsDescriptionsOnly: past the cap a firing trigger loads nothing and is
// recorded once as suppressed, so the run record shows what the cap withheld.
func TestCapKeepsDescriptionsOnly(t *testing.T) {
	seed := []state.Skill{
		triggered("a-skill", callsTo("exec")),
		triggered("b-skill", callsTo("exec")),
		triggered("c-skill", callsTo("exec")),
	}
	_, b, _ := briefSet(t, seed, skilltool.WithLoadPolicy(skillload.Policy{MaxTriggered: 2}))

	out := brief(t, b, nil, runTests)
	want := []string{"trigger:a-skill", "trigger:b-skill", "suppressed:c-skill"}
	if got := vias(out); !equal(got, want) {
		t.Fatalf("notes = %v, want %v", got, want)
	}
	if strings.Contains(out.Text, "c-skill") {
		t.Fatalf("the capped skill's body was loaded: %q", out.Text)
	}
	if again := brief(t, b, out.State, runTests); len(again.Notes) != 0 {
		t.Fatalf("a second turn re-reported %v, want each suppression once", vias(again))
	}
}

// TestModelReadSuppressesTheTrigger: a skill the model already read is not loaded again.
func TestModelReadSuppressesTheTrigger(t *testing.T) {
	set, b, _ := briefSet(t, []state.Skill{triggered("feedback-loop", repeat3)})
	read := mission.ObservedCall{Tool: "skill_read", Input: json.RawMessage(`{"skill":"feedback-loop"}`)}

	out := brief(t, b, nil, read, runTests, runTests, runTests)
	if out.Text != "" || len(out.Notes) != 0 {
		t.Fatalf("loaded %q %v after the model read the skill itself", out.Text, out.Notes)
	}
	if len(set.Loaded()) != 0 {
		t.Fatalf("Loaded() = %v, want none", set.Loaded())
	}
}

// TestFailedModelReadDoesNotSuppress: a read that errored put nothing in the conversation.
func TestFailedModelReadDoesNotSuppress(t *testing.T) {
	_, b, _ := briefSet(t, []state.Skill{triggered("feedback-loop", repeat3)})
	read := mission.ObservedCall{Tool: "skill_read", Input: json.RawMessage(`{"skill":"feedback-loop"}`), IsError: true}

	out := brief(t, b, nil, read, runTests, runTests, runTests)
	if !strings.Contains(out.Text, "feedback-loop") {
		t.Fatalf("a failed read suppressed the trigger, text = %q", out.Text)
	}
}

// TestStartLoadsInTheOrderNamed: named skills load in the first turn in the order the
// host gave them, and a name that cannot load is skipped and recorded while the rest load.
func TestStartLoadsInTheOrderNamed(t *testing.T) {
	seed := []state.Skill{triggered("alpha"), triggered("beta"), triggered("needs-shell"), triggered("empty")}
	seed[3].Body = ""
	check := func(sk state.Skill) (bool, string) {
		if sk.Slug == "needs-shell" {
			return false, "requires tools not mounted: shell"
		}
		return true, ""
	}
	set, b, ids := briefSet(t, seed,
		skilltool.WithStart("beta", "ghost", "needs-shell", "empty", "alpha", "beta"),
		skilltool.WithStartCheck(check))

	out, err := b.Brief(context.Background(), mission.BriefInput{Start: true})
	if err != nil {
		t.Fatalf("Brief: %v", err)
	}
	if bi, ai := strings.Index(out.Text, "Procedure for beta."), strings.Index(out.Text, "Procedure for alpha."); bi < 0 || ai < 0 || bi > ai {
		t.Fatalf("beta must load before alpha, text = %q", out.Text)
	}
	if strings.Count(out.Text, "Procedure for beta.") != 1 {
		t.Fatalf("a name listed twice loaded twice: %q", out.Text)
	}
	want := []string{"start:beta", "skipped:ghost", "skipped:needs-shell", "skipped:empty", "start:alpha"}
	if got := vias(out); !equal(got, want) {
		t.Fatalf("notes = %v, want %v", got, want)
	}
	for _, n := range out.Notes {
		if n.Via == "skipped" && n.Reason == "" {
			t.Errorf("skipped %s has no reason", n.Name)
		}
	}
	if got := set.Loaded(); len(got) != 2 || got[0] != ids["beta"] || got[1] != ids["alpha"] {
		t.Fatalf("Loaded() = %v, want beta then alpha", got)
	}

	// A start skill is loaded, so a later trigger would not load it again, and it did not
	// spend the trigger cap.
	var st skillload.State
	if err := json.Unmarshal(out.State, &st); err != nil {
		t.Fatalf("state: %v", err)
	}
	if !st.Has("alpha") || !st.Has("beta") || st.Triggered != 0 {
		t.Fatalf("state = %+v, want both loaded and no trigger spent", st)
	}
}

func TestStartedSkillIsNotTriggeredAgain(t *testing.T) {
	_, b, _ := briefSet(t, []state.Skill{triggered("feedback-loop", repeat3)}, skilltool.WithStart("feedback-loop"))
	start, err := b.Brief(context.Background(), mission.BriefInput{Start: true})
	if err != nil {
		t.Fatalf("Brief: %v", err)
	}
	out := brief(t, b, start.State, runTests, runTests, runTests)
	if out.Text != "" {
		t.Fatalf("a skill loaded at start loaded again by trigger: %q", out.Text)
	}
}

func TestUnreadableStateLoadsNothing(t *testing.T) {
	_, b, _ := briefSet(t, []state.Skill{triggered("feedback-loop", callsTo("exec"))})
	if _, err := b.Brief(context.Background(), mission.BriefInput{State: json.RawMessage(`{"loaded":`), Calls: []mission.ObservedCall{runTests}}); err == nil {
		t.Fatal("a state that cannot be read must not load: the loaded set is unknown")
	}
}

// TestSkillsWithoutTriggersAreNeverLoaded: a skill with no declared trigger stays model-read only.
func TestSkillsWithoutTriggersAreNeverLoaded(t *testing.T) {
	_, b, _ := briefSet(t, []state.Skill{triggered("plain")})
	if out := brief(t, b, nil, runTests, runTests, runTests); out.Text != "" || len(out.Notes) != 0 {
		t.Fatalf("loaded %q %v for a skill with no triggers", out.Text, out.Notes)
	}
}
