package goal

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Non-convergence is the third way a run can go nowhere. The budget guard catches a run
// that spent its allowance and the no-progress guard (progress.go) one that stopped doing
// anything. Neither catches a run that is busy, spending, and told the same thing every
// cycle, which looks exactly like one that is working. Repeated pressure that has stopped
// being informative is also a safety risk, so the goal stops instead of applying it again.
//
// Repetition is keyed on the normalized substance of the refusal, paired with how much of
// the ledger stood proven when it was given, so an item flipping to proven resets the
// count however the refusal is worded. It is not keyed on the progress fingerprint: a run
// thrashing through new files changes that every step, and is the run this catches.
const (
	// VerdictRepeatLimit is how many consecutive build-and-check cycles may end in the
	// same refusal before the goal stops. The first refusal is information and the second
	// says the cycle in between did not change it, so a third would cost a step to learn
	// nothing.
	VerdictRepeatLimit = 2
)

// ObserveVerdict folds one refusal into the repetition count and returns the count after
// it. stopReason is what the stop evaluator said when it declined to call the goal done,
// and itemDetail is what the current item's own check last reported. Either may be
// empty; they are compared together because a change in either is the run being told
// something new.
//
// A refusal with no substance is not observed: counting silence as repetition would stop
// every goal driven by an evaluator that returns a bare false, as Flynn's shipped
// evaluators do. The count starts at one on a refusal whose substance is new, so the
// limit reads as the number of cycles that ended the same way.
func (s *Status) ObserveVerdict(stopReason, itemDetail string) int {
	mark := verdictMark(stopReason, itemDetail, s.provenCount())
	if mark == "" {
		return s.VerdictRepeat // nothing was stated; there is nothing to compare
	}
	s.LastVerdict = verdictText(stopReason, itemDetail)
	if mark != s.VerdictMark {
		s.VerdictMark = mark
		s.VerdictRepeat = 1
		return 1
	}
	s.VerdictRepeat++
	return s.VerdictRepeat
}

// ExecutedFeedback is the current item's failed-check detail when that check actually
// ran, and "" when it did not. Only that is counted for non-convergence.
//
// The two look identical on the status and mean opposite things. A check that ran and
// failed is the run being told its work is not done, and repeating it is the signal this
// file is about. A check that could not be run reports on the host: on a machine that
// cannot contain a model-authored command every check comes back unrunnable with the same
// wording forever, and counting that would stop every goal on such a host whether or not
// it was converging. The distinction is the recorded verdict's provenance, the evidence
// the gate refuses a claim on.
func (s Status) ExecutedFeedback(ledger []LedgerItem, recorded []Verification) string {
	if s.ItemFeedback == "" {
		return ""
	}
	item, ok := s.CurrentItem(ledger)
	if !ok {
		return ""
	}
	// Recorded verifications arrive in the order they were written, so the last one for
	// this item is the check the feedback describes.
	for i := len(recorded) - 1; i >= 0; i-- {
		if recorded[i].Item == item.ID {
			if recorded[i].Provenance == ProvenanceExecuted {
				return s.ItemFeedback
			}
			return ""
		}
	}
	return ""
}

// StalledForNonConvergence reports whether the run has been refused for the same reason
// often enough that another cycle will not change it.
func (s Status) StalledForNonConvergence() bool {
	return s.VerdictRepeat >= VerdictRepeatLimit
}

// NonConvergenceReason is the stall message. It quotes what the run was told, says how
// many cycles it was told it, and asks the question only an operator can answer: a goal
// that stopped converging is where a human helps, and dying silently wastes what the run
// did establish.
func (s Status) NonConvergenceReason() string {
	return fmt.Sprintf(
		"not converging: %d consecutive cycles ended in the same refusal, %q, with no ledger item proven in between. "+
			"Nothing the run did changed that verdict, so another step will not either. "+
			"Does the stop condition still hold, or does it need restating?",
		s.VerdictRepeat, s.LastVerdict)
}

// provenCount is how many ledger items currently stand proven. Unlike a progress
// fingerprint, which moves whenever the run touches anything, it moves only when the run
// has satisfied part of its own definition of done. With no ledger it stays zero and the
// refusal's wording is the only signal, as it should be.
func (s Status) provenCount() int {
	n := 0
	for _, st := range s.Ledger {
		if st.Proven {
			n++
		}
	}
	return n
}

// verdictMark builds the comparison key: the normalized refusal paired with the proven
// count, or "" when nothing was stated.
func verdictMark(stopReason, itemDetail string, proven int) string {
	norm := normalizeVerdict(stopReason) + "\x00" + normalizeVerdict(itemDetail)
	if norm == "\x00" {
		return ""
	}
	return strconv.Itoa(proven) + "\x00" + norm
}

// verdictText is the human-readable form of what the run was told, for the stall message.
func verdictText(stopReason, itemDetail string) string {
	switch {
	case stopReason != "" && itemDetail != "":
		return stopReason + "; " + itemDetail
	case stopReason != "":
		return stopReason
	default:
		return itemDetail
	}
}

// normalizeVerdict reduces a refusal to the substance two of them are compared on: case,
// punctuation and spacing carry no meaning, so a rephrased complaint is still repeated.
//
// Digits are kept. "3 tests still failing" then "2 tests still failing" is a run being
// told something new, and stripping the numbers would turn the clearest evidence of
// progress into a repetition.
func normalizeVerdict(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
			continue
		}
		space = true
	}
	return b.String()
}
