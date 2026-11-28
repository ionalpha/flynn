package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"testing"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// How `flynn resume` comes by each of a run's levers. A "restored" one is rebuilt from the
// run's record; any other value says where it comes from or why it is not rebuilt.
const restored = "restored from the record"

var resumeLevers = map[string]string{
	"budget":        restored,
	"planning":      restored,
	"proof":         restored,
	"gates":         restored,
	"shell":         "this invocation's flags",
	"selection":     "this invocation's flags",
	"model":         "resolved again by this invocation: --model, the saved default, or asked",
	"allowances":    "persisted on the run's goal",
	"terms":         "persisted on the run's goal",
	"stopCondition": "persisted on the run's goal",
	"extAgent":      "resume refuses an external agent's run",
	"toolset":       "only a review sets it, and a review is not resumed",
	"observe":       "only a review sets it, and a review is not resumed",
	"skills":        "a resumed run offers no skills",
	"loaded":        "a resumed run offers no skills",
	"emb":           "ranks recall, which a resume does not do",
	"project":       "not re-read: a resume runs on the default system prompt",
}

// TestResumeAccountsForEveryLever fails when a lever is added to a run without deciding
// what a resume does with it, then checks the restored ones come back equal.
func TestResumeAccountsForEveryLever(t *testing.T) {
	var levers []string
	for f := range reflect.TypeFor[driveConfig]().Fields() {
		levers = append(levers, f.Name)
		if resumeLevers[f.Name] == "" {
			t.Errorf("driveConfig.%s: say in resumeLevers what a resume does with it", f.Name)
		}
	}
	for name := range resumeLevers {
		if !slices.Contains(levers, name) {
			t.Errorf("resumeLevers names %s, which driveConfig does not have", name)
		}
	}

	for name, fanout := range map[string]*fanoutConfig{"single": nil, "fanout": {}} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store := memStore(t)
			reg, err := governed.Registry()
			if err != nil {
				t.Fatal(err)
			}
			base := driveRequest{
				out: &bytes.Buffer{}, model: llmtest.NewScripted(llmtest.SayText("done")), workdir: t.TempDir(),
				objective: "do the task", system: defaultSystemPrompt, rstore: store.Resources(reg), jq: store.Jobs(), log: store.Log(),
			}
			opts := []driveOption{
				withShellPolicy(checkShell),
				withBudget(budgetpkg.Limits{Tokens: 400_000, Cost: 0.5}),
				withApproval([]string{"bash"}, nil), withAllowance([]string{"write"}, nil),
			}
			fresh := base
			fresh.fanout = fanout
			if fanout == nil {
				opts = append(opts, withPlanning(), withLedgerProof())
			}
			fresh = fresh.withOptions(opts...)
			first, err := fresh.assembly().assemble(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := first.Posture()
			_ = first.Close()

			resumed := base
			resumed.resumeID = first.ID()
			resumed, err = resumed.resuming(ctx, t.TempDir(), checkShell)
			if err != nil {
				t.Fatal(err)
			}
			if (resumed.fanout != nil) != (fanout != nil) {
				t.Fatalf("resumed fan-out = %v", resumed.fanout != nil)
			}
			second, err := resumed.assembly().assemble(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = second.Close() })
			if got := second.Posture(); !reflect.DeepEqual(got, want) {
				t.Errorf("resumed posture\n%+v\nwant\n%+v", got, want)
			}
			was, now := reflect.ValueOf(fresh.cfg), reflect.ValueOf(resumed.cfg)
			for lever, how := range resumeLevers {
				// A fan-out plans and proves by its mode, so its resume may say so explicitly.
				if how != restored || (fanout != nil && (lever == "planning" || lever == "proof")) {
					continue
				}
				// Unexported fields compare through their printed form.
				if w, g := fmt.Sprint(was.FieldByName(lever)), fmt.Sprint(now.FieldByName(lever)); w != g {
					t.Errorf("driveConfig.%s: resumed %v, fresh %v", lever, g, w)
				}
			}
		})
	}
}

type failingModel struct{ err error }

func (m failingModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, m.err
}

var announcedRun = regexp.MustCompile(`(?m)^\s+run (\S+)$`)

// spendingTurn is a tool call that reports usage, so a token ceiling has something to count.
func spendingTurn() llm.Response {
	r := llmtest.CallTool("1", "glob", []byte(`{"pattern":"*"}`))
	r.Usage = llm.Usage{InputTokens: 100, OutputTokens: 100}
	return r
}

// TestEveryWayARunEndsSealsIt injects a failure on each path out of a run and requires a
// record that verifies and says how the run ended.
func TestEveryWayARunEndsSealsIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   llm.Model
		opts    []driveOption
		outcome string
		fails   bool
	}{
		{name: "converged", model: llmtest.NewScripted(llmtest.SayText("done"))},
		{name: "model error", model: failingModel{errors.New("injected provider failure")}, fails: true},
		{
			name: "budget stop", fails: true,
			model: llmtest.NewScripted(spendingTurn(), llmtest.SayText("done")),
			opts:  []driveOption{withBudget(budgetpkg.Limits{Tokens: 1})}, outcome: resultBudget,
		},
		{name: "script runs out", model: llmtest.NewScripted(), fails: true},
		{
			name: "breached term", model: llmtest.NewScripted(llmtest.SayText("done")),
			opts: []driveOption{withGoalSpec(goalSpecFile{Invariants: []goal.Invariant{{
				ID: "never", Statement: "this term does not hold", Check: "exit 1",
			}}}), withShellPolicy(checkShell)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, store := t.Context(), memStore(t)
			var out bytes.Buffer
			_, err := runLearningMission(ctx, &out, tc.model, harness.Plan{}, nil, t.TempDir(), "do the task", "",
				store, selfCertifyingSigner(t), false, nil, tc.opts...)
			if tc.fails && err == nil {
				t.Fatalf("the injected failure did not stop the run:\n%s", out.String())
			}
			m := announcedRun.FindStringSubmatch(out.String())
			if m == nil {
				t.Fatalf("no run id announced:\n%s", out.String())
			}
			var report bytes.Buffer
			if verr := verifyStoredRun(ctx, &report, store, m[1]); verr != nil {
				t.Fatalf("run ended with %v and left no verifiable record: %v\n%s", err, verr, out.String())
			}
			got := recordedOutcome(t, ctx, store, m[1])
			if err != nil && (got == "" || got == chain.ResultSuccess) {
				t.Errorf("run failed with %v and recorded outcome %q", err, got)
			}
			if tc.outcome != "" && got != tc.outcome {
				t.Errorf("outcome = %q, want %q", got, tc.outcome)
			}
		})
	}
}
