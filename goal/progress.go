package goal

import (
	"context"
	"fmt"

	"github.com/ionalpha/flynn/resource"
)

// No-progress detection is the signal a reconciler otherwise lacks: not "budget
// exhausted" or "stop condition met", but this step accomplished nothing. Without it a
// goal that loops re-reading the same files spends its whole step budget and lands in a
// budget stall, which reads as bad luck rather than as the loop noticing it was stuck.
//
// The shape is an idle streak: increment when a step changed nothing, reset on any
// progress, stop at a limit. What counts as progress is deliberately multi-signal
// (a tool run, a file touched, a ledger item proven, a commit) so a step that legitimately
// advances the work in any of those ways is not read as idle.
//
// The one rule every recorded failure of this idea broke: do not key progress on the
// working tree. A circuit breaker fired on a productive agent because it had committed
// its work and left a clean tree; a git-rev-parse gate in a non-git workspace root
// disabled the file-change check entirely, so nothing ever read as progress; a checkbox
// regex counted a date like [2026-01-29] as unfinished work; a stale-loop check killed a
// real workflow after three task.complete events because it keyed on the event name, not
// on whether anything different had happened. The fix is to compare a fingerprint of the
// substantive signals the run actually recorded — including commits — and to reset on any
// of them changing. That is what a ProgressProbe returns; this file is the streak logic
// over it, kept in the goal package so it is pure and testable without a runtime.

const (
	// NoProgressLimit is how many consecutive steps a goal may make no substantive
	// change before it is stopped for lack of progress. It is small on purpose: a run
	// that has changed nothing for three straight steps is looping, not thinking, and
	// every additional idle step is spend with nothing to show for it. It sits well below
	// DefaultMaxSteps so a stuck goal is caught by the loop noticing rather than by the
	// budget running out.
	NoProgressLimit = 3
	// progressWarnAt is the streak at which the goal is first told it is stalling: one
	// step before the limit, because a goal told it is stalling sometimes changes course
	// and un-stalls itself, and a warning that arrives only as the run is killed is no
	// warning at all.
	progressWarnAt = 2
)

// ObserveProgress folds one completed step's progress fingerprint into the idle streak
// and returns the streak after this step. A fingerprint that differs from the last
// recorded mark is progress: the streak resets to zero and the new mark is stored. An
// identical fingerprint is an idle step: the streak increments. Either way the step's
// summary is recorded as what the goal was last doing, so a stall can name it.
//
// The first observed step is always progress: there is no prior mark to be idle against,
// so a goal is never charged an idle step for the one that established the baseline. This
// relies on the probe returning a non-empty fingerprint even when nothing substantive
// happened (a stable encoding of "no signals"); an empty fingerprint would read as the
// baseline step forever and the goal would never stall. That contract is stated on
// ProgressProbe.
func (s *Status) ObserveProgress(fingerprint, summary string) int {
	s.LastActivity = summary
	if s.ProgressMark == "" || fingerprint != s.ProgressMark {
		s.ProgressMark = fingerprint
		s.IdleStreak = 0
		return 0
	}
	s.IdleStreak++
	return s.IdleStreak
}

// StalledForNoProgress reports whether the idle streak has reached the limit: the goal
// has made no substantive change for NoProgressLimit consecutive steps and must stop.
func (s Status) StalledForNoProgress() bool {
	return s.IdleStreak >= NoProgressLimit
}

// NoProgressReason is the stall message, naming what the goal was last doing when it
// stopped, so the record says "stuck doing X" rather than the budget reason a
// no-progress loop would otherwise reach. It is the reason a no-progress stall carries.
func (s Status) NoProgressReason() string {
	if s.LastActivity == "" {
		return fmt.Sprintf("no progress for %d consecutive steps", s.IdleStreak)
	}
	return fmt.Sprintf("no progress for %d consecutive steps; last doing: %s", s.IdleStreak, s.LastActivity)
}

// ProgressWarning returns the nudge to hand the agent when it is stalling but not yet
// stopped: from progressWarnAt up to (but not including) the limit, a message naming the
// streak. It returns "" below the warning point, and "" at the limit, where the goal is
// stopped rather than warned. Kept here so the exact wording is one string the reconciler
// stamps and the executor delivers.
func ProgressWarning(streak int) string {
	if streak < progressWarnAt || streak >= NoProgressLimit {
		return ""
	}
	return fmt.Sprintf("Idle streak: %d step(s) without progress. Make a substantive change or the run will stop.", streak)
}

// ProgressProbe computes a fingerprint of the substantive work observable for a goal as
// of now, over the durable record the runtime keeps — steps and tool calls recorded on
// the spine, files touched, ledger items advanced, and git commits — and never the
// working tree alone, the single choice every recorded failure of no-progress detection
// shares. The reconciler compares successive fingerprints: an unchanged one is an idle
// step, a changed one is progress on some signal. Summary is a short description of the
// last thing the goal did, recorded so a no-progress stall can name it.
//
// A probe must return a non-empty fingerprint even when nothing substantive happened —
// a stable encoding of the empty signal set — so two do-nothing steps compare equal and
// the streak advances. Returning "" reads as the baseline-establishing first step every
// time (see ObserveProgress) and the goal would never stall. A probe error is the
// reconciler's to classify: a transient read failure should not be a terminal stall.
type ProgressProbe interface {
	Progress(ctx context.Context, r resource.Resource) (fingerprint, summary string, err error)
}
