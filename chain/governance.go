package chain

import (
	"math"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// The governance vocabulary: lifecycle events the execution waist emits for every
// governed action. An action is admitted (GovStart), then completes (GovEnd), or
// is refused before it runs (GovRejected); GovCallKey holds the correlation id
// pairing one invocation's events. These strings are the wire contract; an
// integration test asserts a producer agrees with them.
const (
	GovStart     = "dispatch.start"
	GovEnd       = "dispatch.end"
	GovRejected  = "dispatch.rejected"
	GovCallKey   = "call"
	GovActionKey = "action"
	// GovErrorClassKey is how a caller should react to a refusal or failure.
	GovErrorClassKey = "error_class"
	// GovErrorCodeKey is which rule refused. Several gates share one class, so the
	// class alone cannot tell three refusals by one gate from one by each of three.
	GovErrorCodeKey = "error_code"
)

// Governance failure codes, matching the published Provetrail registry.
const (
	CodeUnadmittedAction  = "gov.unadmitted_action"
	CodeDeniedButExecuted = "gov.admission_denied_but_executed"
)

// VerifyGovernance checks the admission invariants (L4) over a run's events. The
// events must already be verified and in order. It fails on:
//
//   - a completion (end) with no earlier admission (start) for its call, which
//     claims an action ran unauthorized;
//   - a call both refused (rejected) and completed, which claims a denied action
//     executed.
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

	// A completion and a refusal for one call contradict in either order, so check
	// the full sets after the pass.
	//ordered:ok every contradicting call gives the same error
	for call := range completed {
		if refused[call] {
			return fault.New(fault.Terminal, CodeDeniedButExecuted,
				"chain: an action that was denied admission also completed")
		}
	}
	return nil
}

func callID(payload map[string]any) (int64, bool) {
	return intField(payload, GovCallKey)
}

// intField reads an integer across the integer and float shapes a CBOR or JSON
// round trip produces, so a record verifies after either.
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
