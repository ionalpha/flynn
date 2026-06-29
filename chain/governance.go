package chain

import (
	"math"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// The governance vocabulary a record carries: the lifecycle event types the
// execution waist emits for every governed action. An action is admitted before it
// runs (GovStart), then completes (GovEnd), or is refused before it runs
// (GovRejected); GovCallKey is the payload key holding the correlation id that pairs
// one invocation's events. These string values are the wire contract a producer
// writes and this verifier reads; an integration test asserts a producer agrees with
// them, so the two cannot silently drift.
const (
	GovStart    = "dispatch.start"
	GovEnd      = "dispatch.end"
	GovRejected = "dispatch.rejected"
	GovCallKey  = "call"
)

// Governance failure codes, matching the published Provetrail registry.
const (
	CodeUnadmittedAction  = "gov.unadmitted_action"
	CodeDeniedButExecuted = "gov.admission_denied_but_executed"
)

// VerifyGovernance checks the admission invariants over a run's events, the
// governance (L4) tier above the cryptographic record. It assumes the events are
// already authentic and in order (verify the run record first), and checks what the
// signed bytes then mean:
//
//   - No action completed without a preceding admission. Every completion (end) names
//     a call that was admitted (a start) earlier in the stream. A record carrying a
//     completion with no admission is rejected: it claims an action ran that was never
//     authorized.
//   - No denied action also completed. No call appears as both refused (rejected) and
//     completed (end). A record carrying both for one call is rejected: it claims an
//     action that was denied admission nonetheless executed.
//
// Passing it means the recorded actions are consistent with having been admitted
// through the governance gate: the record is not just untampered, it is governed.
func VerifyGovernance(events []spine.Event) error {
	admitted := map[int64]bool{}
	completed := map[int64]bool{}
	refused := map[int64]bool{}

	for _, e := range events {
		call, ok := callID(e.Payload)
		switch e.Type {
		case GovStart:
			if ok {
				admitted[call] = true
			}
		case GovEnd:
			if !ok || !admitted[call] {
				return fault.New(fault.Terminal, CodeUnadmittedAction,
					"chain: an action completed with no preceding admission")
			}
			if refused[call] {
				return fault.New(fault.Terminal, CodeDeniedButExecuted,
					"chain: an action that was denied admission also completed")
			}
			completed[call] = true
		case GovRejected:
			if ok {
				refused[call] = true
			}
		}
	}

	// A completion and a refusal for the same call are contradictory regardless of the
	// order they appear in, so check the full sets once more after the pass.
	for call := range completed {
		if refused[call] {
			return fault.New(fault.Terminal, CodeDeniedButExecuted,
				"chain: an action that was denied admission also completed")
		}
	}
	return nil
}

// callID extracts the correlation id from a lifecycle event's payload.
func callID(payload map[string]any) (int64, bool) {
	return intField(payload, GovCallKey)
}

// intField reads an integer-valued payload field tolerantly across the integer and
// float representations a CBOR or JSON round trip can produce, so a record is
// verifiable whether its events came straight from the canonical bytes or back
// through a store that serializes them as JSON.
func intField(payload map[string]any, key string) (int64, bool) {
	v, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
