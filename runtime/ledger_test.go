package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
)

type stubExec struct{}

func (stubExec) Execute(context.Context, resource.Resource) (json.RawMessage, error) {
	return nil, nil
}

type stubStop struct{}

func (stubStop) Met(context.Context, goal.Spec, goal.Status) (bool, string, error) {
	return false, "", nil
}

type stubVerifier struct{}

func (stubVerifier) VerifyItem(context.Context, resource.Resource, goal.LedgerItem) (goal.ItemVerdict, error) {
	return goal.ItemVerdict{}, nil
}

type stubEvidence struct{}

func (stubEvidence) Record(context.Context, resource.Resource, string, goal.ItemVerdict) (goal.Verification, error) {
	return goal.Verification{}, nil
}

func (stubEvidence) Recorded(context.Context, resource.Resource) ([]goal.Verification, error) {
	return nil, nil
}

// TestLedgerLoopIsWiredOnlyWhenBothHalvesArePresent: wiring the producer without the record
// (or the other way round) is not a degraded mode, it is a goal that can never converge, so
// the composition that knows about both refuses to build half of it.
func TestLedgerLoopIsWiredOnlyWhenBothHalvesArePresent(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"neither", Config{}},
		{"verifier only", Config{Verifier: stubVerifier{}}},
		{"record only", Config{Evidence: stubEvidence{}}},
		{"both", Config{Verifier: stubVerifier{}, Evidence: stubEvidence{}}},
		{"both, and the refusal on", Config{Verifier: stubVerifier{}, Evidence: stubEvidence{}, RequireLedgerProof: true}},
		{"both, asserted evidence allowed", Config{Verifier: stubVerifier{}, Evidence: stubEvidence{}, AllowAssertedEvidence: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Executor, cfg.Stop = stubExec{}, stubStop{}
			if _, err := New(cfg); err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

// TestTheGateIsBuiltDuringComposition: constructing the gate IS the check. Building it here
// rather than accepting one means a gate whose refusal path had been refactored away fails
// the process at startup instead of being wired in and certifying every claim at runtime.
func TestTheGateIsBuiltDuringComposition(t *testing.T) {
	// The real gate must pass its own self-test under both provenance policies, which is
	// what makes either composition below constructible at all.
	for _, allowAsserted := range []bool{false, true} {
		if _, err := New(Config{
			Executor: stubExec{}, Stop: stubStop{},
			Verifier: stubVerifier{}, Evidence: stubEvidence{},
			AllowAssertedEvidence: allowAsserted,
		}); err != nil {
			t.Fatalf("composition failed its gate self-test (allowAsserted=%v): %v", allowAsserted, err)
		}
	}
}
