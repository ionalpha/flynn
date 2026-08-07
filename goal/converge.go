package goal

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Non-convergence is the third way a run can be going nowhere, and the one a reconciler
// that knows only "budget exhausted / condition met / keep going" cannot see. The budget
// guard catches a run that has spent its allowance. The no-progress guard (progress.go)
// catches a run that has stopped doing anything. Neither catches the run that is busy,
// spending, producing steps, and being told the same thing every single time, because a
// goal that became unsatisfiable at step 4 looks exactly like a goal that is working.
//
// The recorded cost of not having this: one goal run whose condition had quietly become
// impossible spent 200+ evaluations over five hours and roughly half a weekly token
// budget, re-injecting identical "not satisfied" feedback every turn while the run
// produced nothing. It was not idle, so no idle-streak check would have saved it. It was
// under budget until it was not. The only signal available the whole time was that the
// refusal never changed.
//
// It is also a safety signal and not only a spend one. The subjective-condition version of
// the same incident applied the same pressure nine turns running against explicit user
// instructions to the contrary. A gate that keeps telling a run it has not done enough is
// a gate that will eventually push a less well-behaved run into doing something it was
// told not to, so the point at which the pressure stops being informative is the point at
// which it should stop being applied.
//
// What repetition is keyed on is the whole design. The recorded failure to avoid: a
// stale-loop detector fired on three consecutive task.complete events and killed a
// legitimate workflow that was correctly emitting one per task, because it keyed on the
// name of what happened rather than on whether the substance differed. So the key here is
// the substance of the refusal, normalized, paired with how much of the ledger stood
// proven when it was given. An item flipping to proven is the run advancing against its
// own definition of done, and it resets the count no matter how the refusal is worded.
// That is what keeps N legitimately similar cycles that are each real work from tripping
// it, and it is deliberately not keyed on the progress fingerprint: a run thrashing
// through new files every step has a fingerprint that changes every step, which is the
// exact run this guard exists to catch.

const (
	// VerdictRepeatLimit is how many consecutive build-and-check cycles may end in the
	// same refusal before the goal stops. Two is the whole point rather than a cautious
	// default: the first refusal is information, and the second says the cycle in between
	// did not change it, so the third would cost a step to learn nothing. Anything larger
	// is paying to confirm what two already established.
	VerdictRepeatLimit = 2
)

// ObserveVerdict folds one refusal into the repetition count and returns the count after
// it. stopReason is what the stop evaluator said when it declined to call the goal done,
// and itemDetail is what the current ledger item's own check last reported; either may be
// empty, and they are compared together because a change in either is the run being told
// something new.
//
// A refusal with no substance at all is not observed. An evaluator that declines without
// saying why has given nothing to compare, and counting silence as repetition would stop
// every goal driven by an evaluator that returns a bare false. Flynn's own shipped
// evaluators do exactly that, so this is the common case and not an edge one: the signal
// costs nothing where no reason is stated and does nothing there either.
//
// The count starts at one on a refusal whose substance is new, so the limit reads as the
// number of cycles that ended the same way rather than the number of repeats after the
// first.
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
// ran, and "" when it did not. It is what non-convergence may count; the raw feedback is
// not.
//
// The two look identical on the status and mean opposite things. A check that ran and
// reported a failure is the run being told its work is not done, and repeating it is the
// signal this file is about. A check that could not be run at all reports on the host, not
// on the work: on a machine that cannot contain a model-authored command every check comes
// back unrunnable with the same wording forever, and counting that would stop every goal on
// such a host for the one reason that has nothing to do with whether it was converging.
//
// The distinction is the recorded verdict's provenance rather than anything re-derived
// here, so it is the same evidence the gate refuses a claim on.
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
// many cycles it was told it, and asks the question an operator is the only one who can
// answer, because a goal that has stopped converging is the exact case where a human
// would help and dying quietly wastes the one thing the run did establish.
func (s Status) NonConvergenceReason() string {
	return fmt.Sprintf(
		"not converging: %d consecutive cycles ended in the same refusal, %q, with no ledger item proven in between. "+
			"Nothing the run did changed that verdict, so another step will not either. "+
			"Does the stop condition still hold, or does it need restating?",
		s.VerdictRepeat, s.LastVerdict)
}

// provenCount is how many ledger items currently stand proven. It is the advance the
// refusal is keyed against: unlike a progress fingerprint, which moves whenever the run
// touches anything, this moves only when the run has satisfied part of its own definition
// of done. A goal with no ledger holds this at zero, which leaves the refusal's own
// wording as the only signal, correctly: there is nothing else to go on.
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
// count. Returns "" when nothing was stated, which is the signal to observe nothing.
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

// normalizeVerdict reduces a refusal to the substance two of them are compared on: case
// and punctuation and spacing carry no meaning here, so a model that rephrases the same
// complaint is still repeating it.
//
// Digits are deliberately kept. "3 tests still failing" followed by "2 tests still
// failing" is a run being told something new, and stripping the numbers would turn the
// clearest evidence of progress there is into a repetition and stop a run that was
// converging.
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
