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
	// ErrEvidenceAsserted reports a claim whose only backing verification was asserted
	// rather than executed, against a gate that requires execution. It is distinct from
	// ErrNoEvidence because it means something different about the item: the check was
	// not unavailable, it was not run.
	ErrEvidenceAsserted = errors.New("goal: the item's only verification was asserted, not executed")
)

// Provenance is how a verdict was arrived at. It is a second axis alongside Passed, not
// a confidence attached to it, and the distinction decides something in three places: the
// gate can require execution and refuse an assertion outright, a regrade can
// re-adjudicate an executed record and can never re-adjudicate an assertion, and a run
// report can state the mix rather than averaging two different things into one number.
//
// The second-order reason is the one that matters most. A verify clause that cannot be
// mechanically run is usually a clause written badly. Recording the fallback as a
// permanently weaker kind is what keeps pressure on the planner to write runnable
// clauses; a fallback recorded identically to an execution removes that pressure
// entirely, and the degradation then happens silently.
type Provenance string

// The two kinds of evidence, mirroring the chain vocabulary. They are kinds, not tiers:
// there is no promotion, no override, and no "close enough" between them.
const (
	// ProvenanceExecuted is a verdict the producer reached by running the item's check.
	ProvenanceExecuted Provenance = chain.ProvenanceExecuted
	// ProvenanceAsserted is a verdict nothing was run for.
	ProvenanceAsserted Provenance = chain.ProvenanceAsserted
)

// Verification is one recorded verdict of a ledger item's declared check, read off the
// run's spine. Ref is the recording's unique identity (its spine sequence), which is
// what the gate consumes so a verification is spent once; Item is the ledger item id the
// check ran against; Passed is the verdict; Provenance is whether the check was run or
// only claimed.
type Verification struct {
	Ref        string
	Item       string
	Passed     bool
	Provenance Provenance
}

// VerificationsFrom reads the item verifications a run recorded on its spine, in the
// order they were recorded. A malformed event — one with no item id — is skipped rather
// than trusted: the gate is default-FAIL, so a verification it cannot read is a
// verification that does not count, never one that is waved through.
//
// An event with a verdict but no readable provenance is not malformed enough to skip
// (it carries the item and the verdict, which is everything the original contract had),
// so it is taken at its weakest reading and counted as asserted. Default-FAIL applied to
// the second axis: the only value that buys an event the executed kind is that exact
// value, present and readable on the event itself.
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
			Ref:        strconv.FormatInt(e.Seq, 10),
			Item:       item,
			Passed:     passed,
			Provenance: provenanceOf(e.Payload[chain.ItemProvenanceKey]),
		})
	}
	return out
}

// provenanceOf reads an event's provenance field, resolving anything that is not
// literally the executed marker (absent, a non-string, a value from a future vocabulary
// this build does not know) to ProvenanceAsserted.
func provenanceOf(v any) Provenance {
	if s, ok := v.(string); ok && s == chain.ProvenanceExecuted {
		return ProvenanceExecuted
	}
	return ProvenanceAsserted
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
type EvidenceGate struct {
	// requireExecuted refuses a claim whose only backing verification was asserted. It
	// is the policy that makes the provenance axis decide something rather than label it: an
	// unrunnable verify clause then fails the item instead of quietly passing it.
	requireExecuted bool
}

// GateOption configures an EvidenceGate.
type GateOption func(*EvidenceGate)

// RequireExecuted makes the gate admit only a verification the producer actually ran.
// An asserted verification never satisfies such a gate: there is no promotion, no
// override, and no tier between the two, because a kind that can be converted into the
// other on request is a weaker instance wearing a different label.
func RequireExecuted() GateOption { return func(g *EvidenceGate) { g.requireExecuted = true } }

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
// evidence is admitted, and, when it is configured to require execution, that an
// asserted claim is refused too.
func NewEvidenceGate(opts ...GateOption) (*EvidenceGate, error) {
	g := &EvidenceGate{}
	for _, o := range opts {
		o(g)
	}
	if err := selfTest(g.admit, g.requireExecuted); err != nil {
		return nil, err
	}
	return g, nil
}

// admit is the gate's decision, factored to a value so the self-test drives the exact
// path the runtime does. It scans the recorded verifications for the item and returns the
// ref of the first passing, unconsumed, admissible one: the single admitting outcome.
// Falling off the loop is a refusal, never an admission, which is what makes the gate
// default-FAIL by construction rather than by a flag.
//
// The three refusals are ordered by how specific they are about what went wrong: a spent
// verification is the most informative (something did prove this item, once), then an
// asserted one under an executing gate (a check exists but was not run), then no evidence
// at all. A run record that flattened these into one reason would lose the distinction
// between an item nobody checked and an item whose check could not be run.
func (g *EvidenceGate) admit(itemID string, recorded []Verification, consumed map[string]bool) (string, error) {
	spent, asserted := false, false
	for _, v := range recorded {
		if v.Item != itemID || !v.Passed {
			continue
		}
		if g.requireExecuted && v.Provenance != ProvenanceExecuted {
			asserted = true
			continue
		}
		if consumed[v.Ref] {
			spent = true
			continue
		}
		return v.Ref, nil
	}
	switch {
	case spent:
		return "", fmt.Errorf("%w: %s", ErrEvidenceSpent, itemID)
	case asserted:
		return "", fmt.Errorf("%w: %s", ErrEvidenceAsserted, itemID)
	default:
		return "", fmt.Errorf("%w: %s", ErrNoEvidence, itemID)
	}
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

// ProveRecorded settles the ledger against what the run's record actually shows: every
// still-unproven item the gate admits flips to proven, in ledger order, consuming its
// verification as it goes. It returns how many items flipped.
//
// A refusal is not an error here and is not reported as one. Most items are unproven
// because their work has not been done yet, so a pass that proves nothing is the normal
// state of a run in progress; the place a refusal becomes a verdict is the convergence
// gate, where an item still unproven at the point the goal wants to finish is named with
// its reason (UnprovenReasons).
//
// It is deliberately a fold over the whole ledger rather than a claim about one item.
// The producer records a verification and stops; this reads the record and decides. So a
// crash between recording and proving costs nothing: the event is durable, and the next
// reconcile settles it. The item state is a projection of the spine, which is the only
// arrangement where the two cannot drift.
func (s *Status) ProveRecorded(g *EvidenceGate, recorded []Verification, now time.Time) int {
	proved := 0
	for _, id := range s.Unproven() {
		if err := s.Prove(g, id, recorded, now); err == nil {
			proved++
		}
	}
	return proved
}

// UnprovenReasons names every still-unproven ledger item and why the gate would refuse
// it, in ledger order. It is what a convergence refusal says instead of "not done": a
// reader needs to know whether an item has no recorded check at all, has one that was
// already spent on another item, or has one that was asserted rather than run, because
// those are three different problems with three different fixes.
func (s Status) UnprovenReasons(g *EvidenceGate, recorded []Verification) []string {
	unproven := s.Unproven()
	if len(unproven) == 0 {
		return nil
	}
	consumed := s.consumedRefs()
	out := make([]string, 0, len(unproven))
	for _, id := range unproven {
		_, err := g.admit(id, recorded, consumed)
		out = append(out, id+" ("+unprovenReason(err)+")")
	}
	return out
}

// unprovenReason renders one gate refusal as the short phrase UnprovenReasons splices in.
// A nil error means the gate would have admitted the item, which reaching this function
// at all says did not happen on the settling pass, so it is reported as the anomaly it is
// rather than papered over as "not done".
func unprovenReason(err error) string {
	switch {
	case err == nil:
		return "admissible but not settled"
	case errors.Is(err, ErrEvidenceSpent):
		return "its verification was already consumed by another item"
	case errors.Is(err, ErrEvidenceAsserted):
		return "its verification was asserted, not executed"
	default:
		return "no recorded passing verification"
	}
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

// selfTest asserts, in-process, that a decision enforces the gate's contract on every one
// of its outcomes: a claim with no evidence is refused, a claim backed only by a failing
// check is refused, a claim backed by a fresh passing check is admitted and names that
// check, and a claim whose passing check was already consumed is refused as spent. Any
// deviation means the gate is broken and it returns ErrGateBroken naming what it caught.
// Running against admitFunc rather than the method is what lets the fail-loud test feed it
// a broken decision and confirm selfTest does not pass a gate that would wave claims
// through.
//
// requireExecuted selects which provenance behaviour is asserted, because the two
// policies have opposite correct answers for an asserted check and a self-test that
// checked neither would pass a gate whose whole provenance branch had been deleted. The
// executed check must be admitted under both, so the axis can never turn into a way to
// refuse the evidence the producer actually ran.
func selfTest(admit admitFunc, requireExecuted bool) error {
	const item = "selftestitem0000"
	// 1. No evidence at all: must refuse for want of evidence.
	if _, err := admit(item, nil, nil); !errors.Is(err, ErrNoEvidence) {
		return fmt.Errorf("%w: a claim with no evidence was not refused (got %s)", ErrGateBroken, errText(err))
	}
	// 2. A failing check is not evidence: must refuse.
	failing := []Verification{{Ref: "1", Item: item, Passed: false, Provenance: ProvenanceExecuted}}
	if _, err := admit(item, failing, nil); !errors.Is(err, ErrNoEvidence) {
		return fmt.Errorf("%w: a failing check was accepted as evidence (got %s)", ErrGateBroken, errText(err))
	}
	// 3. A fresh passing executed check: must admit and name it, under either policy.
	passing := []Verification{{Ref: "42", Item: item, Passed: true, Provenance: ProvenanceExecuted}}
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
	// 5. The provenance axis, in whichever direction this gate was configured for. An
	// executing gate must refuse an assertion outright (that refusal is the only thing
	// that stops an unrunnable verify clause from passing silently), and a gate that was
	// not configured for it must still admit one, so turning the policy on stays a
	// decision rather than something the gate does on its own.
	asserted := []Verification{{Ref: "7", Item: item, Passed: true, Provenance: ProvenanceAsserted}}
	switch _, err := admit(item, asserted, nil); {
	case requireExecuted && !errors.Is(err, ErrEvidenceAsserted):
		return fmt.Errorf("%w: an asserted check satisfied a gate requiring execution (got %s)", ErrGateBroken, errText(err))
	case !requireExecuted && err != nil:
		return fmt.Errorf("%w: an asserted check was refused by a gate that does not require execution (%s)", ErrGateBroken, errText(err))
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
