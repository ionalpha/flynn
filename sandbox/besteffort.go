package sandbox

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
)

// floorPolicy is whether a Local may run a command at the process-jail floor when the
// host refuses the confinement it asked for. It is off unless [WithBestEffortConfinement]
// turns it on, and every use is counted and reported, so a host running below the tier it
// was configured for is visible rather than silent.
type floorPolicy struct {
	optedIn bool
	notice  func(reason error)
	runs    atomic.Int64
}

// WithBestEffortConfinement is the named opt-in to running below the confinement a Local
// is configured for. It applies [WithDefaultConfinement], and when the host refuses that
// confinement it runs the command at the process-jail floor (paths confined and the
// environment scrubbed, with no kernel boundary) instead of refusing.
//
// Each such run is reported to notice with the reason the confinement failed (on Linux a
// [RefusedError], which names the cause and the fix), and counted in
// [Local.ConfinementFallbacks]. A nil notice writes the reason to standard error, so the
// opt-in cannot be silent. Governed egress, a network denial, a set of paths hidden with
// [WithUnreadablePaths] and an explicit read grant are never relaxed by it. It is for development on a host that cannot confine; leave it
// out wherever the work is semi-trusted, since [WithDefaultConfinement] alone refuses.
func WithBestEffortConfinement(notice func(reason error)) LocalOption {
	return func(l *Local) {
		WithDefaultConfinement()(l)
		l.floor.optedIn = true
		l.floor.notice = notice
	}
}

// BestEffortConfinement reports whether this Local was built with
// [WithBestEffortConfinement], so a record of the run can say its confinement may have
// been lowered.
func (l *Local) BestEffortConfinement() bool { return l.floor.optedIn }

// ConfinementFallbacks counts the launches that ran at the process-jail floor because the
// host refused the confinement this Local was configured for. It is always zero unless
// [WithBestEffortConfinement] was used.
func (l *Local) ConfinementFallbacks() int64 { return l.floor.runs.Load() }

// floorPermitted reports whether a launch that failed with err may run at the floor: the
// Local opted in, holds no governed egress or network denial (which fail loudly rather
// than open the network) and no read-deny set (which the floor cannot apply), and err is
// not a refused read grant.
func (l *Local) floorPermitted(err error) bool {
	return l.floor.optedIn && l.egress == nil && !l.denyNetwork && len(l.unreadable) == 0 &&
		!errors.Is(err, ErrReadGrant)
}

// fallBack reports whether a confined launch that did not start may be run again at the
// floor, and records the run when it may. It is the one place the decision is made for
// Exec, Capture, Stream, Serve and Session, so no launch path has a fallback of its own.
// A Local that reports the kernel-confined tier never falls back: the trust gate admitted
// semi-trusted code on that guarantee, so a launch failure after a passing probe has to
// refuse. The failed attempt never ran the command, so running it again is safe.
func (l *Local) fallBack(err error, confined bool) bool {
	if err != nil && confined {
		// Whatever the cause, the pass the gate remembered no longer describes the host.
		l.forgetConfinementPass()
	}
	if err == nil || !confined || !l.floorPermitted(err) || l.kernelConfinementEnforceable() {
		return false
	}
	l.floor.record(err)
	return true
}

// fallBackUnexpressible is [Local.fallBack] for a platform that cannot apply confinement
// to a backgrounded process at all, where there is no launch error to inspect.
func (l *Local) fallBackUnexpressible(reason error) bool {
	if !l.floorPermitted(reason) {
		return false
	}
	l.floor.record(reason)
	return true
}

// record counts a run at the floor and reports why to the notice.
func (p *floorPolicy) record(reason error) {
	p.runs.Add(1)
	if p.notice != nil {
		p.notice(reason)
		return
	}
	fmt.Fprintf(os.Stderr, "flynn: confinement refused, running a command at the process-jail floor with no kernel boundary: %v\n", reason)
}
