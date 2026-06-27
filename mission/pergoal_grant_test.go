package mission

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// driveSpec runs a goal carrying the given spec (its per-goal grant included) to
// convergence through exec, carrying the checkpoint across steps. It is the
// per-goal analogue of driveToDone, which uses a fixed grant-less spec.
func driveSpec(t *testing.T, exec *Executor, spec goal.Spec, maxSteps int) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var prev json.RawMessage
	for i := 1; i <= maxSteps; i++ {
		st := goal.Status{Checkpoint: prev}
		enc, err := st.Encode()
		if err != nil {
			t.Fatal(err)
		}
		r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: raw, Status: enc}
		next, err := exec.Execute(context.Background(), r)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		prev = next
		met, _, err := Convergence{}.Met(context.Background(), spec, goal.Status{Checkpoint: next})
		if err != nil {
			t.Fatal(err)
		}
		if met {
			return
		}
	}
	t.Fatalf("did not converge within %d steps", maxSteps)
}

// TestPerGoalGrantGovernsAuthority proves authority travels with the goal: the
// grant carried on the goal decides what its run may do, it overrides the
// executor's default grant, and with no goal grant the run falls back to the
// executor default. This is the foundation least-privilege delegation needs: one
// executor can drive a parent at full authority and a child narrowed to a subset.
func TestPerGoalGrantGovernsAuthority(t *testing.T) {
	cases := []struct {
		name        string
		execDefault capability.Grant // WithGrant on the executor (zero = none)
		hasDefault  bool
		goalGrant   []string // goal.Spec.Grant
		wantEcho    bool
	}{
		// No executor grant: the goal's grant is the sole authority.
		{"goal grant allows", capability.Grant{}, false, []string{"echo", ActionModelGenerate}, true},
		{"goal grant denies", capability.Grant{}, false, []string{"read", ActionModelGenerate}, false},
		// Goal grant overrides a narrower executor default (delegation widens nothing,
		// but the goal's own authority, not the executor's, is what governs it).
		{"goal grant overrides exec default", capability.NewGrant("read", ActionModelGenerate), true, []string{"echo", ActionModelGenerate}, true},
		// No goal grant: fall back to the executor default (back-compat).
		{"falls back to exec default", capability.NewGrant("echo", ActionModelGenerate), true, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var runs int32
			tool := Func(echoDef, func(_ context.Context, in json.RawMessage) (string, error) {
				atomic.AddInt32(&runs, 1)
				return string(in), nil
			})
			rec := &recordingReporter{}
			model := llmtest.NewScripted(
				llmtest.CallTool("c1", "echo", json.RawMessage(`{"x":1}`)),
				llmtest.SayText("done"),
			)
			opts := []Option{WithTools(tool), WithObserver(rec)}
			if tc.hasDefault {
				opts = append(opts, WithGrant(tc.execDefault))
			}
			exec := NewExecutor(model, opts...)

			driveSpec(t, exec, goal.Spec{
				Objective:     "echo something",
				StopCondition: "done",
				Grant:         tc.goalGrant,
			}, 5)

			ran := atomic.LoadInt32(&runs) == 1
			if ran != tc.wantEcho {
				t.Fatalf("echo ran=%v, want %v", ran, tc.wantEcho)
			}
			if res := firstOfKind(rec.events(), EventToolResult); res != nil {
				if tc.wantEcho && res.IsError {
					t.Fatalf("granted echo should not error: %q", res.Result)
				}
				if !tc.wantEcho && (!res.IsError || !strings.Contains(res.Result, "capability grant")) {
					t.Fatalf("ungranted echo should be a capability denial, got IsError=%v %q", res.IsError, res.Result)
				}
			}
		})
	}
}
