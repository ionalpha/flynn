package mission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// TestExecuteRejectsUndecodableState checks that a status or checkpoint the executor
// cannot read is a terminal failure that names which of the two was bad, and that the
// model is never called: retrying a corrupt record would burn a turn per attempt
// without the record ever becoming readable.
func TestExecuteRejectsUndecodableState(t *testing.T) {
	spec, err := json.Marshal(goal.Spec{Objective: "do the thing", StopCondition: "it is done"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		status   json.RawMessage
		wantCode string
	}{
		{"status is not an object", json.RawMessage(`[1]`), "mission_status_decode"},
		{"checkpoint is not an object", json.RawMessage(`{"checkpoint":[1]}`), "mission_checkpoint_decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := llmtest.NewScripted(llmtest.SayText("unreachable"))
			r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: spec, Status: tc.status}

			_, err := NewExecutor(model).Execute(context.Background(), r)

			if err == nil {
				t.Fatal("Execute accepted state it cannot decode")
			}
			if got := fault.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %q, want %q (err: %v)", got, tc.wantCode, err)
			}
			if got := fault.Classify(err); got != fault.Terminal {
				t.Fatalf("class = %v, want terminal", got)
			}
			if n := len(model.Requests()); n != 0 {
				t.Fatalf("the model was called %d times for an unreadable record", n)
			}
		})
	}
}
