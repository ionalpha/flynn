package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// unsealedRun drives a run to convergence on the plain log, as `flynn resume` does, and
// returns its id. Nothing has sealed it.
func unsealedRun(t *testing.T, ctx context.Context, store *sqlite.Store) string {
	t.Helper()
	reg, err := governed.Registry()
	if err != nil {
		t.Fatal(err)
	}
	_, runID, _, err := driveRequest{
		out: io.Discard, model: llmtest.NewScripted(llmtest.SayText("done")), plan: harness.Plan{},
		workdir: t.TempDir(), objective: "reply done and stop", system: defaultSystemPrompt,
		rstore: store.Resources(reg), jq: store.Jobs(), log: store.Log(),
	}.execute(ctx)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	return runID
}

// TestAResumedRunIsSealed: resume drives a run whose earlier events another process wrote,
// so it has no recorder to seal from, and before this the resumed run was left with no
// record at all and `flynn spine verify` refused it. It is sealed from the durable stream,
// and a run that stopped says how it ended inside the sealed chain.
func TestAResumedRunIsSealed(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	signer := selfCertifyingSigner(t)

	for _, tc := range []struct {
		name    string
		runErr  error
		outcome string
	}{
		{name: "converged"},
		{name: "stalled", runErr: errors.New("goal stalled: no progress"), outcome: resultStalled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := unsealedRun(t, ctx, store)
			var out bytes.Buffer
			sealResumed(ctx, &out, store, runID, signer, tc.runErr)
			if !strings.Contains(out.String(), "run sealed") {
				t.Fatalf("no seal reported:\n%s", out.String())
			}
			var report bytes.Buffer
			if err := verifyStoredRun(ctx, &report, store, runID); err != nil {
				t.Fatalf("verify the resumed run: %v\n%s", err, report.String())
			}
			if got := recordedOutcome(t, ctx, store, runID); got != tc.outcome {
				t.Fatalf("recorded outcome = %q, want %q", got, tc.outcome)
			}
		})
	}
}

// TestAnInterruptedResumeIsLeftOpen: Ctrl-C on a resume detaches, and the run can be
// resumed again, so it is neither sealed nor marked as stopped.
func TestAnInterruptedResumeIsLeftOpen(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	runID := unsealedRun(t, ctx, store)

	var out bytes.Buffer
	sealResumed(ctx, &out, store, runID, selfCertifyingSigner(t), fmt.Errorf("drive: %w", context.Canceled))
	if out.Len() != 0 {
		t.Fatalf("an interrupted resume reported: %s", out.String())
	}
	if err := verifyStoredRun(ctx, io.Discard, store, runID); err == nil {
		t.Fatal("an interrupted resume was sealed")
	}
	if got := recordedOutcome(t, ctx, store, runID); got != "" {
		t.Fatalf("an interrupted resume recorded outcome %q", got)
	}
}

// recordedOutcome is the result of the last outcome event on the run's stream, or "".
func recordedOutcome(t *testing.T, ctx context.Context, store *sqlite.Store, runID string) string {
	t.Helper()
	events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, e := range events {
		if e.Type == chain.OutcomeRecorded {
			got, _ = e.Payload[chain.OutcomeResultKey].(string)
		}
	}
	return got
}
