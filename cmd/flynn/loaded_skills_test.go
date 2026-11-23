package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/state"
)

// A loaded skill is in the first request whether or not recall would offer it. The
// objective shares no word with the skill, so recall does not list it, and only the
// option can put the body there; the run without the option shows that.
func TestLoadedSkillIsInTheFirstRequest(t *testing.T) {
	const body = "Number every step before starting the first."
	for name, tc := range map[string]struct {
		opts []driveOption
		want bool
	}{
		"loaded":     {[]driveOption{withLoadedSkills("step-order")}, true},
		"not loaded": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			store := memStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := store.Skills().Upsert(ctx, state.Skill{Slug: "step-order", Name: "step-order", Body: body}); err != nil {
				t.Fatal(err)
			}
			model := llmtest.NewScripted(llmtest.SayText("done"))
			var out bytes.Buffer
			if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, t.TempDir(), "tidy the readme", "", store, nil, false, nil, tc.opts...); err != nil {
				t.Fatalf("run: %v", err)
			}
			reqs := model.Requests()
			if len(reqs) == 0 {
				t.Fatal("the run made no model call")
			}
			if got := strings.Contains(reqs[0].System, body); got != tc.want {
				t.Errorf("first request carries the body: %v, want %v\n%s", got, tc.want, reqs[0].System)
			}
		})
	}
}

// A skill that cannot be loaded stops the run before it spends anything, with an error
// a measurement can tell apart from a failed attempt.
func TestUnloadableSkillStopsBeforeAnyModelCall(t *testing.T) {
	store := memStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	model := llmtest.NewScripted(llmtest.SayText("done"))
	var out bytes.Buffer
	_, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, t.TempDir(), "tidy the readme", "", store, nil, false, nil, withLoadedSkills("never-installed"))
	if !errors.Is(err, errSkillNotLoaded) {
		t.Fatalf("err = %v, want errSkillNotLoaded", err)
	}
	if n := model.Calls(); n != 0 {
		t.Errorf("the run made %d model calls before refusing", n)
	}
}
