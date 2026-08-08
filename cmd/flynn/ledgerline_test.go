package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// A capability that is available and off is invisible unless the run says so.
// Requiring proof is the one staged verdict in the boundary register: the loop runs
// and records verdicts on every planned goal, and the refusal that reads them is
// behind a flag. An operator has to be able to see both halves of that from the run
// itself, not only from the register.
func TestLedgerLine(t *testing.T) {
	for _, tc := range []struct {
		name            string
		planned, proof  bool
		want, wantNoted string
	}{
		{name: "an unplanned goal has no ledger to speak about"},
		{
			name:      "planned, proof off: the checks run and the dial is named",
			planned:   true,
			want:      "each item's check runs and is recorded",
			wantNoted: "--require-proof",
		},
		{
			name:    "planned, proof on: an unproven item stops the run",
			planned: true, proof: true,
			want: "stops the run",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ledgerLine(tc.planned, tc.proof)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("ledgerLine(%v, %v) = %q, want nothing", tc.planned, tc.proof, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("ledgerLine(%v, %v) = %q, want it to mention %q", tc.planned, tc.proof, got, tc.want)
			}
			if tc.wantNoted != "" && !strings.Contains(got, tc.wantNoted) {
				t.Fatalf("ledgerLine(%v, %v) = %q, does not name the switch %q", tc.planned, tc.proof, got, tc.wantNoted)
			}
			// The proof-on line must not read as an advertisement for the flag that is
			// already set, and the proof-off line must not claim a stop that will not happen.
			if tc.proof && strings.Contains(got, "--require-proof") {
				t.Errorf("the line offers a flag that is already on: %q", got)
			}
		})
	}
}

// And the run actually prints it, so the line is not a function nobody calls.
func TestAPlannedRunSaysWhatItsLedgerWillDo(t *testing.T) {
	dir := t.TempDir()
	model := llmtest.NewScripted(
		llmtest.SayText(`[{"item":"write hello.txt","verify":"test -f hello.txt"}]`),
		llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi"}`)),
		llmtest.SayText("done"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer

	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, nil, dir, "create hello.txt", "", memStore(t), nil, false, nil, withPlanning()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "--require-proof") {
		t.Fatalf("a planned run did not say its ledger proof is available and off:\n%s", out.String())
	}
}
