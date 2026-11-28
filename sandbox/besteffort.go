package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/ionalpha/flynn/fault"
)

// floorPolicy counts and reports every run at the process-jail floor, so a host
// below its configured tier is visible.
type floorPolicy struct {
	optedIn bool
	notice  func(reason error)
	runs    atomic.Int64
}

// WithBestEffortConfinement applies [WithDefaultConfinement], and runs at the
// process-jail floor (no kernel boundary) when the host refuses it. Each such run
// goes to notice (on Linux a [RefusedError]) and [Local.ConfinementFallbacks]; a
// nil notice writes to standard error. Governed egress, a network denial,
// [WithUnreadablePaths] and a read grant are never relaxed. Use it only for
// development; semi-trusted work needs the refusal.
func WithBestEffortConfinement(notice func(reason error)) LocalOption {
	return func(l *Local) {
		WithDefaultConfinement()(l)
		l.floor.optedIn = true
		l.floor.notice = notice
	}
}

// BestEffortConfinement lets a run record say its confinement may have been
// lowered.
func (l *Local) BestEffortConfinement() bool { return l.floor.optedIn }

// ConfinementFallbacks counts launches run at the floor; it is zero without
// [WithBestEffortConfinement].
func (l *Local) ConfinementFallbacks() int64 { return l.floor.runs.Load() }

// floorPermitted excludes a read-deny set because the floor cannot apply it.
func (l *Local) floorPermitted(err error) bool {
	return l.floor.optedIn && l.egress == nil && !l.denyNetwork && len(l.unreadable) == 0 &&
		!errors.Is(err, ErrReadGrant)
}

// fallBack decides the floor retry for every launch path. A Local reporting the
// kernel-confined tier never falls back, because the trust gate admitted
// semi-trusted code on that guarantee. The retry is safe because the failed
// attempt never ran the command.
func (l *Local) fallBack(err error, confined bool) bool {
	// A cancelled or expired context is the caller stopping the launch, which says
	// nothing about what the host can enforce.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if err != nil && confined {
		// The remembered pass no longer describes the host.
		l.forgetConfinementPass()
	}
	if err == nil || !confined || !l.floorPermitted(err) || l.kernelConfinementEnforceable() {
		return false
	}
	l.floor.record(err)
	return true
}

// fallBackUnexpressible is [Local.fallBack] where a backgrounded process cannot be
// confined, so there is no launch error to inspect.
func (l *Local) fallBackUnexpressible(reason error) bool {
	if !l.floorPermitted(reason) {
		return false
	}
	l.floor.record(reason)
	return true
}

// guardUnsupported refuses [WithDefaultConfinement] where the option sets nothing,
// so the command does not run unbounded without an error.
func (l *Local) guardUnsupported() error {
	if !l.confineUnsupported {
		return nil
	}
	err := fault.New(fault.Forbidden, "sandbox_confine_unsupported",
		"sandbox: kernel confinement is not supported on this platform; refusing rather than running the command unconfined (use the container or microVM tier)")
	if l.fallBackUnexpressible(err) {
		return nil
	}
	return err
}

func (p *floorPolicy) record(reason error) {
	p.runs.Add(1)
	if p.notice != nil {
		p.notice(reason)
		return
	}
	fmt.Fprintf(os.Stderr, "flynn: confinement refused, running a command at the process-jail floor with no kernel boundary: %v\n", reason)
}
