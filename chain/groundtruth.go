package chain

import (
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// The ground-truth vocabulary. A claimed outcome is an OutcomeRecorded event and
// the verification backing it a CheckRecorded event, referenced by id. These
// strings are the wire contract between producer and verifier.
const (
	OutcomeRecorded = "outcome.recorded"
	CheckRecorded   = "check.recorded"

	OutcomeResultKey = "result"
	// ResultSuccess is the claim that requires a backing passing check.
	ResultSuccess = "success"
	// CheckRefKey is, on an outcome, the id of its grounding check and, on a check,
	// its own id.
	CheckRefKey    = "check"
	CheckPassedKey = "passed"
)

// CodeNoGroundTruth is the ground-truth failure code, matching the published
// Provetrail registry.
const CodeNoGroundTruth = "shallow.no_ground_truth"

// VerifyGroundTruth rejects a success outcome with no check in the same run that
// it names and that passed. Signing alone does not make a claim true. Other
// outcomes need no check. The events must already be verified and in order.
func VerifyGroundTruth(events []spine.Event) error {
	passed := map[int64]bool{}
	for _, e := range events {
		if e.Type != CheckRecorded {
			continue
		}
		id, ok := intField(e.Payload, CheckRefKey)
		if !ok {
			continue
		}
		if verdict, _ := e.Payload[CheckPassedKey].(bool); verdict {
			passed[id] = true
		}
	}

	for _, e := range events {
		if e.Type != OutcomeRecorded {
			continue
		}
		if result, _ := e.Payload[OutcomeResultKey].(string); result != ResultSuccess {
			continue
		}
		id, ok := intField(e.Payload, CheckRefKey)
		if !ok || !passed[id] {
			return fault.New(fault.Terminal, CodeNoGroundTruth,
				"chain: a success outcome is not grounded in a passing check")
		}
	}
	return nil
}
