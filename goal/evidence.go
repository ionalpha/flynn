package goal

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

var (
	// ErrNoEvidence reports no recorded passing verification for the item.
	ErrNoEvidence = errors.New("goal: item has no recorded passing verification")
	// ErrEvidenceSpent reports a verification another item already consumed.
	ErrEvidenceSpent = errors.New("goal: the verification was already consumed by another item")
	// ErrGateBroken reports a failed self-test. Such a gate is never used, because a
	// trusted gate that passes everything is worse than none.
	ErrGateBroken = errors.New("goal: evidence gate self-test failed")
	// ErrEvidenceAsserted reports, under [RequireExecuted], a check that exists but was
	// not run.
	ErrEvidenceAsserted = errors.New("goal: the item's only verification was asserted, not executed")
)

// Provenance is separate from Passed, so a gate can refuse assertions, a regrade can
// redo only executed records, and a report can count each kind. An asserted verdict
// stays the weaker kind, which keeps pressure on the planner to write runnable checks.
type Provenance string

// The provenance values mirror package chain's.
const (
	ProvenanceExecuted Provenance = chain.ProvenanceExecuted
	// ProvenanceAsserted is a verdict nothing was run for.
	ProvenanceAsserted Provenance = chain.ProvenanceAsserted
)

// Verification is one verdict read off the spine. Ref is its spine sequence, which the
// gate consumes so a verification is spent once.
type Verification struct {
	Ref        string
	Item       string
	Passed     bool
	Provenance Provenance
	// Reason is why nothing was run; empty when executed.
	Reason string
}

// VerificationsFrom skips an event with no item id, because the gate fails closed.
// Anything but an exact executed provenance reads as asserted.
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

func provenanceOf(v any) Provenance {
	if s, ok := v.(string); ok && s == chain.ProvenanceExecuted {
		return ProvenanceExecuted
	}
	return ProvenanceAsserted
}

// EvidenceGate admits an item only on a passing, unspent verification, and consumes it,
// because a prompt does not stop a model claiming done. It fails closed. Binding by
// [ItemID] voids a rewritten item's old evidence. It holds no state: spent refs are read
// from proven items, so a rebuilt gate agrees with the record.
type EvidenceGate struct {
	requireExecuted bool
}

// GateOption configures an [EvidenceGate].
type GateOption func(*EvidenceGate)

// RequireExecuted refuses asserted verifications with no override, because a kind
// convertible on request is the weaker kind relabelled.
func RequireExecuted() GateOption { return func(g *EvidenceGate) { g.requireExecuted = true } }

// NewEvidenceGate self-tests the decision in process and returns [ErrGateBroken] if it
// fails, so a gate refactored into admitting everything cannot be constructed.
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

// admit refuses by falling off the loop, so it fails closed by construction. Refusals
// are ordered spent, asserted, none, so the most informative reason wins.
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

// Prove is the gated way to settle an item; [Status.MarkProven] is the raw transition
// for an already-admitted proof. A refusal leaves the ledger untouched.
func (s *Status) Prove(g *EvidenceGate, itemID string, recorded []Verification, now time.Time) error {
	ref, err := g.admit(itemID, recorded, s.consumedRefs())
	if err != nil {
		return err
	}
	return s.MarkProven(itemID, ref, now)
}

// ProveRecorded returns how many items flipped. A refusal is no error, because most
// unproven items are unfinished work. It refolds the whole record, so a crash between
// recording and proving costs nothing.
func (s *Status) ProveRecorded(g *EvidenceGate, recorded []Verification, now time.Time) int {
	proved := 0
	for _, id := range s.Unproven() {
		if err := s.Prove(g, id, recorded, now); err == nil {
			proved++
		}
	}
	return proved
}

// UnprovenReasons gives each unproven item its own refusal reason, because "not done"
// would make a host that cannot run checks look like a run that did no work.
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

// unprovenReason splits [ErrNoEvidence] into unchecked and unrunnable, because one needs
// more work and the other a check no building fixes. A nil error is an anomaly.
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
			reason = v.Reason // latest wins
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

// consumedRefs reads from the ledger, so a resumed run sees what was spent.
func (s Status) consumedRefs() map[string]bool {
	out := make(map[string]bool, len(s.Ledger))
	for _, st := range s.Ledger {
		if st.Proven && st.Evidence != "" {
			out[st.Evidence] = true
		}
	}
	return out
}

// admitFunc lets selfTest run the real admit path, and a test feed it a broken one.
type admitFunc func(itemID string, recorded []Verification, consumed map[string]bool) (string, error)

// selfTest checks requireExecuted's direction, because the two policies answer an
// asserted check oppositely and checking neither would miss a deleted branch.
func selfTest(admit admitFunc, requireExecuted bool) error {
	const item = "selftestitem0000"
	if _, err := admit(item, nil, nil); !errors.Is(err, ErrNoEvidence) {
		return fmt.Errorf("%w: a claim with no evidence was not refused (got %s)", ErrGateBroken, errText(err))
	}
	failing := []Verification{{Ref: "1", Item: item, Passed: false, Provenance: ProvenanceExecuted}}
	if _, err := admit(item, failing, nil); !errors.Is(err, ErrNoEvidence) {
		return fmt.Errorf("%w: a failing check was accepted as evidence (got %s)", ErrGateBroken, errText(err))
	}
	passing := []Verification{{Ref: "42", Item: item, Passed: true, Provenance: ProvenanceExecuted}}
	ref, err := admit(item, passing, nil)
	if err != nil {
		return fmt.Errorf("%w: a fresh passing check was refused (%s)", ErrGateBroken, errText(err))
	}
	if ref != "42" {
		return fmt.Errorf("%w: admit named the wrong evidence (%q, want %q)", ErrGateBroken, ref, "42")
	}
	if _, err := admit(item, passing, map[string]bool{"42": true}); !errors.Is(err, ErrEvidenceSpent) {
		return fmt.Errorf("%w: a spent check was reused as evidence (got %s)", ErrGateBroken, errText(err))
	}
	asserted := []Verification{{Ref: "7", Item: item, Passed: true, Provenance: ProvenanceAsserted}}
	switch _, err := admit(item, asserted, nil); {
	case requireExecuted && !errors.Is(err, ErrEvidenceAsserted):
		return fmt.Errorf("%w: an asserted check satisfied a gate requiring execution (got %s)", ErrGateBroken, errText(err))
	case !requireExecuted && err != nil:
		return fmt.Errorf("%w: an asserted check was refused by a gate that does not require execution (%s)", ErrGateBroken, errText(err))
	}
	return nil
}

// errText keeps the observed error out of the [ErrGateBroken] chain callers match on. It
// takes nil, because a broken gate may return no error.
func errText(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
