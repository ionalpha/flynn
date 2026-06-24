package mission

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestExecutorEnforcesGrant proves the capability grant gates tool execution at
// the dispatch waist: a tool the grant permits runs, while one it omits is denied
// before it executes. Either way the conversation continues, because a denied call
// comes back as a failed tool result the model can adapt to rather than a fatal
// error.
func TestExecutorEnforcesGrant(t *testing.T) {
	cases := []struct {
		name     string
		grant    capability.Grant
		wantRuns int32
		wantErr  bool
	}{
		// Each grant lists the model action so the conversation can run; the cases
		// differ in which tool, if any, they also permit.
		{"granted", capability.NewGrant("echo", ActionModelGenerate), 1, false},
		{"ungranted", capability.NewGrant("read", ActionModelGenerate), 0, true},
		{"tools_denied", capability.NewGrant(ActionModelGenerate), 0, true},
		{"allow_all", capability.AllowAll(), 1, false},
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
			exec := NewExecutor(model, WithTools(tool), WithObserver(rec), WithGrant(tc.grant))
			driveToDone(t, exec, 5)

			if got := atomic.LoadInt32(&runs); got != tc.wantRuns {
				t.Fatalf("tool ran %d times, want %d", got, tc.wantRuns)
			}

			res := firstOfKind(rec.events(), EventToolResult)
			if res == nil {
				t.Fatal("no tool result event reported")
			}
			if res.IsError != tc.wantErr {
				t.Fatalf("tool result IsError = %v, want %v (result=%q)", res.IsError, tc.wantErr, res.Result)
			}
			if tc.wantErr && !strings.Contains(res.Result, "capability grant") {
				t.Fatalf("denied result = %q, want a capability-grant denial", res.Result)
			}
		})
	}
}

func firstOfKind(evs []Event, kind EventKind) *Event {
	for i := range evs {
		if evs[i].Kind == kind {
			return &evs[i]
		}
	}
	return nil
}
