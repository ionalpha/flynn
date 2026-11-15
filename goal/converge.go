package goal

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Non-convergence catches a busy run told the same thing every cycle, which the budget and
// no-progress stops miss. Repetition is keyed on the normalized refusal and the proven
// count, so proving an item resets it. The progress fingerprint is not used, because a run
// thrashing through new files changes it every step.
const (
	// VerdictRepeatLimit is the identical refusals before a stop, because the second shows
	// the cycle between changed nothing.
	VerdictRepeatLimit = 2
)

// ObserveVerdict returns the repeat count. stopReason and itemDetail are compared together,
// because a change in either is news. An empty refusal is skipped, because the shipped
// evaluators return a bare false. A new refusal counts as one.
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

// ExecutedFeedback returns the item's failure only if its check ran, read from the recorded
// provenance. An unrunnable check reports on the host, and on a host that cannot sandbox
// a command, counting it would stop every goal.
func (s Status) ExecutedFeedback(ledger []LedgerItem, recorded []Verification) string {
	if s.ItemFeedback == "" {
		return ""
	}
	item, ok := s.CurrentItem(ledger)
	if !ok {
		return ""
	}
	// The last record for this item is the check the feedback describes.
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

// StalledForNonConvergence reports whether the count reached [VerdictRepeatLimit].
func (s Status) StalledForNonConvergence() bool {
	return s.VerdictRepeat >= VerdictRepeatLimit
}

// NonConvergenceReason quotes the refusal and asks the operator whether the stop condition
// still holds, because only a person can answer that.
func (s Status) NonConvergenceReason() string {
	return fmt.Sprintf(
		"not converging: %d consecutive cycles ended in the same refusal, %q, with no ledger item proven in between. "+
			"Nothing the run did changed that verdict, so another step will not either. "+
			"Does the stop condition still hold, or does it need restating?",
		s.VerdictRepeat, s.LastVerdict)
}

// provenCount moves only when part of done is satisfied, unlike the progress fingerprint.
func (s Status) provenCount() int {
	n := 0
	for _, st := range s.Ledger {
		if st.Proven {
			n++
		}
	}
	return n
}

func verdictMark(stopReason, itemDetail string, proven int) string {
	norm := normalizeVerdict(stopReason) + "\x00" + normalizeVerdict(itemDetail)
	if norm == "\x00" {
		return ""
	}
	return strconv.Itoa(proven) + "\x00" + norm
}

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

// normalizeVerdict drops case, punctuation and spacing. Digits stay, because "3 failing"
// then "2 failing" is progress.
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
