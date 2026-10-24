package goal

import (
	"context"
	"fmt"

	"github.com/ionalpha/flynn/resource"
)

const (
	// NoProgressLimit is how many consecutive steps a goal may make no substantive
	// change before it is stopped for lack of progress. It is small on purpose: a run
	// idle for three straight steps is looping, and it sits well below DefaultMaxSteps
	// so a stuck goal is caught by the loop and not by the budget running out.
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
// The first observed step is always progress: there is no prior mark to be idle
// against, so the step that established the baseline is never charged as idle. This
// relies on the non-empty fingerprint contract stated on [ProgressProbe].
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
// of now, over the durable record the runtime keeps (steps and tool calls on the spine,
// files touched, ledger items advanced, git commits). The reconciler compares successive
// fingerprints: an unchanged one is an idle step, a changed one is progress. Summary
// describes the last thing the goal did, so a stall can name it.
//
// A probe must not key on the working tree alone: a clean tree after a commit would read
// as no progress, and a non-git workspace root would disable the check.
//
// A probe must return a non-empty fingerprint even when nothing substantive happened (a
// stable encoding of the empty signal set), so two do-nothing steps compare equal and
// the streak advances; "" reads as the baseline step every time (see
// [Status.ObserveProgress]) and the goal never stalls. A probe error is the
// reconciler's to classify: a transient read failure is not a terminal stall.
type ProgressProbe interface {
	Progress(ctx context.Context, r resource.Resource) (fingerprint, summary string, err error)
}
