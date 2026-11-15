package goal

import (
	"context"
	"fmt"

	"github.com/ionalpha/flynn/resource"
)

const (
	// NoProgressLimit is the idle steps before a stop. Three idle steps is a loop, and
	// the limit sits well below DefaultMaxSteps so the loop is caught before the budget.
	NoProgressLimit = 3
	// progressWarnAt is one step before the limit, because a warned goal sometimes
	// changes course.
	progressWarnAt = 2
)

// ObserveProgress returns the idle streak after one step. The summary is kept so a stall
// can name it. The first step sets the baseline and is never idle, which relies on the
// non-empty fingerprint contract on [ProgressProbe].
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

// StalledForNoProgress reports whether the streak reached [NoProgressLimit].
func (s Status) StalledForNoProgress() bool {
	return s.IdleStreak >= NoProgressLimit
}

// NoProgressReason names what the goal was last doing, so the record says what it was
// stuck on.
func (s Status) NoProgressReason() string {
	if s.LastActivity == "" {
		return fmt.Sprintf("no progress for %d consecutive steps", s.IdleStreak)
	}
	return fmt.Sprintf("no progress for %d consecutive steps; last doing: %s", s.IdleStreak, s.LastActivity)
}

// ProgressWarning returns "" outside [progressWarnAt, NoProgressLimit), because at the
// limit the goal is stopped instead. The reconciler and executor share this one string.
func ProgressWarning(streak int) string {
	if streak < progressWarnAt || streak >= NoProgressLimit {
		return ""
	}
	return fmt.Sprintf("Idle streak: %d step(s) without progress. Make a substantive change or the run will stop.", streak)
}

// ProgressProbe fingerprints a goal's durable work; an unchanged fingerprint is an idle
// step. It must not key on the working tree alone, because a clean tree after a commit
// reads as idle and a non-git root disables the check.
//
// The fingerprint must be non-empty even when nothing happened, because "" reads as the
// baseline every time ([Status.ObserveProgress]) and the goal never stalls. A transient
// error is not a terminal stall.
type ProgressProbe interface {
	Progress(ctx context.Context, r resource.Resource) (fingerprint, summary string, err error)
}
