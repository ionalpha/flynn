package goal

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// The evidence gate is the rule that a ledger item flips to proven only when the run's
// own spine record shows the verification the item declared, and that verification is
// then consumed so it cannot certify a second item. It is the structural half of
// completion: the ledger (ledger.go) says what done is, and the gate decides whether
// the record actually shows it.
//
// The failure this exists to stop is the one every agent harness runs into: a model
// under completion pressure marks a feature passing after a green unit test or a curl
// while the thing itself is visibly broken, and asking it not to in the prompt does not
// reliably work. So "done" is not a phrase the model emits; it is a state transition the
// gate will not make without a recorded, passing, unspent check the run itself produced.
//
// The item id does the binding for free. An item's id is the content address of its
// text and its declared verify clause (ItemID), so a verification that names the item id
// is asserting that item's exact declared check — not some other check the run found
// easier to pass. A rewritten item is a different id, so its old evidence no longer
// matches it.

// Evidence-gate errors. They are distinct because they mean different things about the
// claim: no evidence is a claim with nothing recorded behind it, spent evidence is a
// claim leaning on a verification another item already used, and a broken gate is the
// gate's own logic having stopped enforcing the rule.
var (
	// ErrNoEvidence reports a claim to prove an item that the run's record does not back
	// with a passing verification for that item.
	ErrNoEvidence = errors.New("goal: item has no recorded passing verification")
	// ErrEvidenceSpent reports a claim whose only backing verification was already
	// consumed by another item, so it cannot certify this one too.
	ErrEvidenceSpent = errors.New("goal: the verification was already consumed by another item")
	// ErrGateBroken reports that the gate's own self-test failed: its refusal path did
	// not refuse, or its admit path did not admit. A gate that cannot prove it enforces
	// the rule must not be used, because a gate that fails silently is worse than none.
	ErrGateBroken = errors.New("goal: evidence gate self-test failed")
)

// Verification is one recorded verdict of a ledger item's declared check, read off the
// run's spine. Ref is the recording's unique identity (its spine sequence), which is
// what the gate consumes so a verification is spent once; Item is the ledger item id the
// check ran against; Passed is the verdict.
type Verification struct {
	Ref    string
	Item   string
	Passed bool
}

// VerificationsFrom reads the item verifications a run recorded on its spine, in the
// order they were recorded. A malformed event — one with no item id — is skipped rather
// than trusted: the gate is default-FAIL, so a verification it cannot read is a
// verification that does not count, never one that is waved through.
func VerificationsFrom(events []spine.Event) []Verification {
	var out []Verification
	for _, e := range events {
		if e.Type != chain.ItemVerified {
			continue
		}
		item, _ := e.Payload[chain.ItemKey].(string)
		if item == "" {
			continue
		}
		passed, _ := e.Payload[chain.ItemPassedKey].(bool)
		out = append(out, Verification{
			Ref:    strconv.FormatInt(e.Seq, 10),
			Item:   item,
			Passed: passed,
		})
	}
	return out
}

// EvidenceGate admits a claim that a ledger item is proven only when the run's spine
// carries a passing verification recorded for that item which has not already been spent
// on another item, and it consumes that verification when it admits. It is default-FAIL:
// the only path that returns without an error is the one that found a matching, passing,
// unconsumed verification; every other case — no verification, a failing one, an
// already-spent one — is a refusal with the reason stated.
//
// The gate holds no state of its own. Consumption is durable because it is read from the
// ledger itself (the refs already recorded as proven items' evidence), so it survives a
// crash and a resume: a gate rebuilt after a restart consumes exactly what the record
// says was already spent.
type EvidenceGate struct{}

// NewEvidenceGate returns a gate after proving, in this same process, that its decision
// actually enforces the rule. A gate whose admit path was refactored into default-ALLOW,
// or whose refusal stopped refusing, cannot be constructed: it returns ErrGateBroken here
// rather than being wired in and silently certifying every claim at runtime.
//
// This is not ceremony. The reference harness this gate is modeled against shipped a kill
// switch that showed as loaded while doing nothing, because the contract it emitted was
// silently dropped downstream. A gate that can fail silently is worse than no gate,
// because it is trusted. So this one refuses to exist unless it has just demonstrated,
// against its own code, that a claim with no evidence is refused and a claim with fresh
// evidence is admitted.
func NewEvidenceGate() (*EvidenceGate, error) {
	g := &EvidenceGate{}
	if err := selfTest(g.admit); err != nil {
		return nil, err
	}
	return g, nil
}

// admit is the gate's decision, factored to a value so the self-test drives the exact
// path the runtime does. It scans the recorded verifications for the item and returns the
// ref of the first passing, unconsumed one — the single admitting outcome. If it finds
// passing evidence but every instance is already spent it refuses as spent; otherwise it
// refuses for want of evidence. Falling off the loop is a refusal, never an admission,
// which is what makes the gate default-FAIL by construction rather than by a flag.
func (g *EvidenceGate) admit(itemID string, recorded []Verification, consumed map[string]bool) (string, error) {
	spent := false
	for _, v := range recorded {
		if v.Item != itemID || !v.Passed {
			continue
		}
		if consumed[v.Ref] {
			spent = true
			continue
		}
		return v.Ref, nil
	}
	if spent {
		return "", fmt.Errorf("%w: %s", ErrEvidenceSpent, itemID)
	}
	return "", fmt.Errorf("%w: %s", ErrNoEvidence, itemID)
}

// Prove admits and records a ledger item as proven under the evidence gate: the item
// flips to proven only if the run's recorded verifications carry a passing, unspent check
// for it, and that check is then consumed by being stored as the item's evidence, so a
// later Prove cannot lean on it again. It is the enforced way to settle a ledger item on
// the run path; MarkProven remains the raw state transition beneath it, for callers (a
// test, a replay) that are recording an already-admitted proof.
//
// A refusal returns the gate's error unchanged and leaves the ledger untouched, so a run
// that cannot prove an item does not half-mark it.
func (s *Status) Prove(g *EvidenceGate, itemID string, recorded []Verification, now time.Time) error {
	ref, err := g.admit(itemID, recorded, s.consumedRefs())
	if err != nil {
		return err
	}
	return s.MarkProven(itemID, ref, now)
}

// consumedRefs is the set of verification refs already spent by proven items, read from
// the ledger state so consumption is a fact about the durable record rather than
// in-memory bookkeeping the next step or a resumed run would not see.
func (s Status) consumedRefs() map[string]bool {
	out := make(map[string]bool, len(s.Ledger))
	for _, st := range s.Ledger {
		if st.Proven && st.Evidence != "" {
			out[st.Evidence] = true
		}
	}
	return out
}

// admitFunc is the gate's decision as a value, so selfTest exercises the real admit path
// and a test can hand it a deliberately broken decision to prove selfTest catches it.
type admitFunc func(itemID string, recorded []Verification, consumed map[string]bool) (string, error)

// selfTest asserts, in-process, that a decision enforces the gate's contract on all four
// of its outcomes: a claim with no evidence is refused, a claim backed only by a failing
// check is refused, a claim backed by a fresh passing check is admitted and names that
// check, and a claim whose passing check was already consumed is refused as spent. Any
// deviation means the gate is broken and it returns ErrGateBroken naming what it caught.
// Running against admitFunc rather than the method is what lets the fail-loud test feed it
// a broken decision and confirm selfTest does not pass a gate that would wave claims
// through.
func selfTest(admit admitFunc) error {
	const item = "selftestitem0000"
	// 1. No evidence at all: must refuse for want of evidence.
	if _, err := admit(item, nil, nil); !errors.Is(err, ErrNoEvidence) {
		return fmt.Errorf("%w: a claim with no evidence was not refused (got %s)", ErrGateBroken, errText(err))
	}
	// 2. A failing check is not evidence: must refuse.
	failing := []Verification{{Ref: "1", Item: item, Passed: false}}
	if _, err := admit(item, failing, nil); !errors.Is(err, ErrNoEvidence) {
		return fmt.Errorf("%w: a failing check was accepted as evidence (got %s)", ErrGateBroken, errText(err))
	}
	// 3. A fresh passing check: must admit and name it.
	passing := []Verification{{Ref: "42", Item: item, Passed: true}}
	ref, err := admit(item, passing, nil)
	if err != nil {
		return fmt.Errorf("%w: a fresh passing check was refused (%s)", ErrGateBroken, errText(err))
	}
	if ref != "42" {
		return fmt.Errorf("%w: admit named the wrong evidence (%q, want %q)", ErrGateBroken, ref, "42")
	}
	// 4. A consumed check cannot prove again: must refuse as spent.
	if _, err := admit(item, passing, map[string]bool{"42": true}); !errors.Is(err, ErrEvidenceSpent) {
		return fmt.Errorf("%w: a spent check was reused as evidence (got %s)", ErrGateBroken, errText(err))
	}
	return nil
}

// errText renders an error for a self-test diagnostic without wrapping it: the observed
// error is evidence of what went wrong, not part of the ErrGateBroken chain callers match
// on, and it may be nil (a broken gate that admitted where it should have refused returns
// no error at all), so this is nil-safe where err.Error() would panic.
func errText(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
