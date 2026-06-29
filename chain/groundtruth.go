package chain

import (
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// The ground-truth vocabulary a record carries. A run that claims an outcome records
// it as an OutcomeRecorded event; a verification that backs an outcome records its
// verdict as a CheckRecorded event. The outcome references the check that grounds it
// by id. These string values are the wire contract a producer writes and this verifier
// reads.
const (
	// OutcomeRecorded is a claimed result of a run or step.
	OutcomeRecorded = "outcome.recorded"
	// CheckRecorded is the verdict of a verification.
	CheckRecorded = "check.recorded"

	// OutcomeResultKey holds the claimed result; ResultSuccess is the positive claim
	// that must be grounded in a passing check.
	OutcomeResultKey = "result"
	// ResultSuccess is the claimed result that requires a backing passing check.
	ResultSuccess = "success"
	// CheckRefKey holds, on an outcome, the id of the check that grounds it, and on a
	// check, that check's own id.
	CheckRefKey = "check"
	// CheckPassedKey holds a check's boolean verdict.
	CheckPassedKey = "passed"
)

// CodeNoGroundTruth is the ground-truth failure code, matching the published
// Provetrail registry.
const CodeNoGroundTruth = "shallow.no_ground_truth"

// VerifyGroundTruth checks that a run's claimed successes are grounded in real
// checks, the difference between a record that is signed and one that is proven. It
// assumes the events are already authentic and in order (verify the run record
// first). For every outcome that claims success, there must be a check, recorded in
// the same run, that the outcome names and whose verdict passed. A success with no
// bound check, or bound to a check that did not pass, is rejected: the record asserts
// an outcome with nothing standing behind it.
//
// Outcomes that do not claim success need no backing check, so a recorded failure or
// a partial result is not penalized for lacking one.
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
