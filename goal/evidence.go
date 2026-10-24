package goal

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// Evidence-gate errors are distinct because they say different things about the claim:
// no evidence is nothing recorded, spent evidence leans on a verification another item
// used, and a broken gate has stopped enforcing the rule.
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
	// ErrEvidenceAsserted reports a claim whose only backing verification was asserted and
	// not executed, against a gate that requires execution. It differs from ErrNoEvidence:
	// the check exists but was not run.
	ErrEvidenceAsserted = errors.New("goal: the item's only verification was asserted, not executed")
)

// Provenance is how a verdict was arrived at. It is a second axis alongside Passed,
// not a confidence attached to it, and it decides three things: the gate can require
// execution and refuse an assertion outright, a regrade can re-adjudicate an executed
// record and never an assertion, and a run report can state the mix rather than
// averaging two different things into one number.
//
// A verify clause that cannot be mechanically run is usually written badly. Recording
// the fallback as a permanently weaker kind keeps pressure on the planner to write
// runnable clauses; recording it identically to an execution removes that pressure and
// the degradation happens silently.
type Provenance string

// The two kinds of evidence, mirroring the chain vocabulary.
const (
	// ProvenanceExecuted is a verdict the producer reached by running the item's check.
	ProvenanceExecuted Provenance = chain.ProvenanceExecuted
	// ProvenanceAsserted is a verdict nothing was run for.
	ProvenanceAsserted Provenance = chain.ProvenanceAsserted
)

// Verification is one recorded verdict of a ledger item's declared check, read off the
// run's spine. Ref is the recording's unique identity (its spine sequence), which the
// gate consumes so a verification is spent once; Item is the ledger item id; Passed is
// the verdict; Provenance is whether the check was run or only claimed.
type Verification struct {
	Ref        string
	Item       string
	Passed     bool
	Provenance Provenance
	// Reason is why nothing was run, on a verification that was not executed; empty on an
	// executed one, whose exit code is what happened.
	Reason string
}

// VerificationsFrom reads the item verifications a run recorded on its spine, in the
// order recorded. An event with no item id is skipped, not trusted: the gate is
// default-FAIL, so a verification it cannot read does not count.
//
// An event with a verdict but no readable provenance is taken at its weakest reading and
// counted as asserted: the only value that buys the executed kind is that exact value,
// present and readable on the event itself.
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
		reason, _ := e.Payload[chain.ItemReasonKey].(string)
		out = append(out, Verification{
			Ref:        strconv.FormatInt(e.Seq, 10),
			Item:       item,
			Passed:     passed,
			Provenance: provenanceOf(e.Payload[chain.ItemProvenanceKey]),
			Reason:     reason,
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
// carries a passing verification recorded for that item and not already spent on
// another, and it consumes that verification when it admits. The ledger says what done
// is; the gate decides whether the record shows it. A model under completion pressure
// marks a feature passing after a green unit test while the thing is visibly broken, and
// a prompt does not reliably stop it.
//
// It is default-FAIL: no verification, a failing one and a spent one are each a refusal
// with the reason stated. A verification binds to its item by id, the content address of
// its text and verify clause ([ItemID]), so a rewritten item's old evidence no longer
// matches. The gate holds no state: consumption is read from the refs recorded as proven
// items' evidence, so a gate rebuilt after a crash consumes exactly what the record says
// was spent.
type EvidenceGate struct {
	// requireExecuted refuses a claim whose only backing verification was asserted, so an
	// unrunnable verify clause fails the item and cannot pass it unseen.
	requireExecuted bool
}

// GateOption configures an EvidenceGate.
type GateOption func(*EvidenceGate)

// RequireExecuted makes the gate admit only a verification the producer actually ran.
// An asserted one never satisfies such a gate: there is no promotion, override or tier
// between the two, because a kind convertible on request is a weaker instance under a
// different label.
func RequireExecuted() GateOption { return func(g *EvidenceGate) { g.requireExecuted = true } }

// NewEvidenceGate returns a gate after proving, in this process, that its decision
// enforces the rule. A gate whose admit path was refactored into default-ALLOW, or whose
// refusal stopped refusing, cannot be constructed: it returns [ErrGateBroken] and is not
// wired in to certify every claim silently.
//
// A reference harness shipped a kill switch that showed as loaded while doing nothing,
// because the contract it emitted was dropped downstream. A gate that can fail silently
// is worse than none, because it is trusted. So this one must first demonstrate against
// its own code that a claim with no evidence is refused, a claim with fresh evidence is
// admitted and, when it requires execution, an asserted claim is refused too.
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
// path the runtime does. It returns the ref of the first passing, unconsumed,
// admissible verification for the item; falling off the loop is a refusal, which makes
// the gate default-FAIL by construction and not by a flag.
//
// The refusals are ordered by how much they say: a spent verification (something proved
// this item once), then an asserted one under an executing gate (a check exists but was
// not run), then no evidence at all. Flattening them would lose the difference between
// an item nobody checked and one whose check could not be run.
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
// flips to proven only if the run's recorded verifications carry a passing, unspent
// check for it, and that check is consumed by being stored as the item's evidence. It
// is the enforced way to settle an item on the run path; [Status.MarkProven] is the raw
// transition beneath it, for callers (a test, a replay) recording an already-admitted
// proof.
//
// A refusal returns the gate's error unchanged and leaves the ledger untouched.
func (s *Status) Prove(g *EvidenceGate, itemID string, recorded []Verification, now time.Time) error {
	ref, err := g.admit(itemID, recorded, s.consumedRefs())
	if err != nil {
		return err
	}
	return s.MarkProven(itemID, ref, now)
}

// ProveRecorded settles the ledger against what the run's record shows: every
// still-unproven item the gate admits flips to proven, in ledger order, consuming its
// verification as it goes. It returns how many items flipped.
//
// A refusal is not an error: most items are unproven because their work is not done,
// and it becomes a verdict at the convergence gate ([Status.UnprovenReasons]).
//
// It folds over the whole ledger because the producer records a verification and stops;
// this reads the record and decides, so a crash between recording and proving costs
// nothing and item state stays a projection of the spine.
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
// it, in ledger order: what a convergence refusal says instead of "not done".
//
// It separates problems with different fixes: nobody checked the item, its check ran and
// failed, its check could not be run, its only passing check was an assertion, or its
// check was spent on another item. "Not done" would make a run whose host could not
// execute its checks look like a run that did no work.
func (s Status) UnprovenReasons(g *EvidenceGate, recorded []Verification) []string {
	unproven := s.Unproven()
	if len(unproven) == 0 {
		return nil
	}
	consumed := s.consumedRefs()
	out := make([]string, 0, len(unproven))
	for _, id := range unproven {
		_, err := g.admit(id, recorded, consumed)
		out = append(out, id+" ("+unprovenReason(err, id, recorded)+")")
	}
	return out
}

// unprovenReason renders one gate refusal as the short phrase UnprovenReasons splices in.
//
// The gate's error is not the whole story: "nobody checked this" and "the check could
// not be run" both reach it as ErrNoEvidence, since an unrunnable check records a verdict
// that did not pass just as a failing one does. This reads the item's recorded
// verifications to tell them apart, because they need opposite responses: one is work
// still to do, the other a check no further building fixes.
//
// A nil error means the gate would have admitted the item, which did not happen on the
// settling pass, so it is reported as the anomaly it is.
func unprovenReason(err error, item string, recorded []Verification) string {
	switch {
	case err == nil:
		return "admissible but not settled"
	case errors.Is(err, ErrEvidenceSpent):
		return "its verification was already consumed by another item"
	case errors.Is(err, ErrEvidenceAsserted):
		return "its verification was asserted, not executed"
	}
	ran, attempted := false, false
	reason := ""
	for _, v := range recorded {
		if v.Item != item {
			continue
		}
		attempted = true
		if v.Provenance == ProvenanceExecuted {
			ran = true
		}
		if v.Reason != "" {
			reason = v.Reason // the latest attempt's account of why nothing ran
		}
	}
	switch {
	case ran:
		return "its check ran and did not pass"
	case attempted && reason != "":
		return "its check could not be run: " + reason
	case attempted:
		return "its check could not be run"
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

// selfTest asserts, in-process, that a decision enforces the gate's contract on every
// outcome: a claim with no evidence is refused, a claim backed only by a failing check
// is refused, a fresh passing check is admitted and named, and a passing check already
// consumed is refused as spent. Any deviation returns [ErrGateBroken] naming what it
// caught. It takes an admitFunc so the fail-loud test can feed it a broken decision.
//
// requireExecuted selects which provenance behaviour is asserted, because the two
// policies have opposite correct answers for an asserted check, and a self-test that
// checked neither would pass a gate whose provenance branch had been deleted. The
// executed check must be admitted under both, so the axis can never refuse evidence the
// producer actually ran.
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
	// executing gate must refuse an assertion outright; any other gate must still admit
	// one, so turning the policy on stays a decision.
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
// error is evidence, not part of the [ErrGateBroken] chain callers match on. It is
// nil-safe because a broken gate that admitted where it should have refused returns no
// error at all.
func errText(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
