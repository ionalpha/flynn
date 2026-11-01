package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/spine"
)

func TestStoppedOutcomeClassifiesHowARunEnded(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"typed budget fault", fault.New(fault.BudgetExceeded, "budget_exceeded", "tokens"), resultBudget},
		{"flattened budget stall", errors.New("goal stalled: budget_exceeded: token ceiling reached"), resultBudget},
		{"operator kill", fmt.Errorf("goal stalled: %s", goal.KillMessage(goal.Kill{Reason: "wrong repo"})), resultKilled},
		{"interrupt", context.Canceled, resultCancelled},
		{"plain stall", errors.New("goal stalled: no progress"), resultStalled},
		{"anything else", errors.New("assembling the run: boom"), resultError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stoppedOutcome(tc.err); got != tc.want {
				t.Fatalf("stoppedOutcome(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestStoppedByIgnoresSuccessAndReadsTheLastFailure(t *testing.T) {
	outcome := func(result, reason string) spine.Event {
		return spine.Event{Type: chain.OutcomeRecorded, Payload: map[string]any{chain.OutcomeResultKey: result, outcomeReasonKey: reason}}
	}
	if _, _, ok := stoppedBy([]spine.Event{outcome(chain.ResultSuccess, "")}); ok {
		t.Fatal("a success outcome was reported as a stop")
	}
	if _, _, ok := stoppedBy(nil); ok {
		t.Fatal("an empty record was reported as a stop")
	}
	result, reason, ok := stoppedBy([]spine.Event{outcome(resultStalled, "first"), outcome(resultKilled, "second")})
	if !ok || result != resultKilled || reason != "second" {
		t.Fatalf("stoppedBy = %q, %q, %v; want the last recorded stop", result, reason, ok)
	}
}

// TestSealStoppedLeavesAVerifiableRecordThatNamesTheStop seals a stream that holds one
// event and a stop, then verifies it from the durable store: the record exists, the
// integrity tier passes, and the report names the stop instead of a success.
func TestSealStoppedLeavesAVerifiableRecordThatNamesTheStop(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}

	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer, err := chain.NewEd25519RootSigner(controlplane.PrincipalID(priv.Public().(ed25519.PublicKey)), priv)
	if err != nil {
		t.Fatal(err)
	}
	rec := chain.NewRecordingLog(store.Log(), nil)
	const runID = "run-stopped"
	if _, err := rec.Append(ctx, spine.AppendInput{Stream: runID, Type: "noise", Actor: spine.ActorAgent}); err != nil {
		t.Fatal(err)
	}

	// An already-cancelled context is what an interrupted run hands over.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	var out bytes.Buffer
	sealStopped(cancelled, &out, store, rec, runID, signer, errors.New("goal stalled: budget_exceeded: tokens"))
	if !strings.Contains(out.String(), "run sealed") {
		t.Fatalf("the stopped run was not sealed:\n%s", out.String())
	}

	var report bytes.Buffer
	if err := verifyStoredRun(ctx, &report, store, runID); err != nil {
		t.Fatalf("verifying the stopped run: %v\n%s", err, report.String())
	}
	for _, want := range []string{"integrity:    VERIFIED", "outcome:      budget_exceeded"} {
		if !strings.Contains(report.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, report.String())
		}
	}
	if strings.Contains(report.String(), "GROUNDED (") {
		t.Fatalf("a stopped run reads as grounded:\n%s", report.String())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
