package chain

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/spine"
)

func checkEvent(id int64, passed bool) spine.Event {
	return spine.Event{Type: CheckRecorded, Payload: map[string]any{CheckRefKey: id, CheckPassedKey: passed}}
}

func outcomeEvent(result string, checkID int64, bound bool) spine.Event {
	p := map[string]any{OutcomeResultKey: result}
	if bound {
		p[CheckRefKey] = checkID
	}
	return spine.Event{Type: OutcomeRecorded, Payload: p}
}

func TestVerifyGroundTruthAccepts(t *testing.T) {
	events := []spine.Event{
		checkEvent(1, true),
		{Type: "tool.call", Payload: map[string]any{}}, // ignored
		outcomeEvent(ResultSuccess, 1, true),
		outcomeEvent("partial", 0, false), // a non-success outcome needs no check
	}
	if err := VerifyGroundTruth(events); err != nil {
		t.Fatalf("a grounded run was rejected: %v", err)
	}
}

func TestVerifyGroundTruthRejectsUnboundSuccess(t *testing.T) {
	err := VerifyGroundTruth([]spine.Event{outcomeEvent(ResultSuccess, 0, false)})
	if govCode(err) != CodeNoGroundTruth {
		t.Fatalf("code = %q, want %q (err: %v)", govCode(err), CodeNoGroundTruth, err)
	}
}

func TestVerifyGroundTruthRejectsFailedCheck(t *testing.T) {
	// Success bound to a check that did not pass.
	err := VerifyGroundTruth([]spine.Event{
		checkEvent(1, false),
		outcomeEvent(ResultSuccess, 1, true),
	})
	if govCode(err) != CodeNoGroundTruth {
		t.Fatalf("code = %q, want %q (err: %v)", govCode(err), CodeNoGroundTruth, err)
	}
}

func TestVerifyGroundTruthRejectsMissingCheck(t *testing.T) {
	// Success bound to a check id that does not appear in the run.
	err := VerifyGroundTruth([]spine.Event{outcomeEvent(ResultSuccess, 99, true)})
	if govCode(err) != CodeNoGroundTruth {
		t.Fatalf("code = %q, want %q (err: %v)", govCode(err), CodeNoGroundTruth, err)
	}
}

// TestVerifyGroundTruthProperties asserts the invariant directly: a run where every
// success is bound to a passing check always verifies, and adding one unbound success
// always fails.
func TestVerifyGroundTruthProperties(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(rt, "outcomes")
		var events []spine.Event
		for i := 1; i <= n; i++ {
			id := int64(i)
			if rapid.Bool().Draw(rt, "success") {
				events = append(events, checkEvent(id, true), outcomeEvent(ResultSuccess, id, true))
			} else {
				events = append(events, outcomeEvent("failure", 0, false))
			}
		}
		if err := VerifyGroundTruth(events); err != nil {
			rt.Fatalf("a grounded run was rejected: %v", err)
		}
		bad := append(append([]spine.Event{}, events...), outcomeEvent(ResultSuccess, int64(n+100), true))
		if govCode(VerifyGroundTruth(bad)) != CodeNoGroundTruth {
			rt.Fatal("unbound success not caught")
		}
	})
}
