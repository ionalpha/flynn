package brakes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
)

// The proof exists to catch a brake that is wired and not enforcing, so every test here is
// about a broken one. Proving that the real brake passes is the easy half and it is first;
// the four after it feed the proof a halt state that is wrong in one specific way and check
// that it says which.

// TestARealBrakeProvesItHalts: the shipped Hook, over its shipped switch, demonstrates its
// own contract. If this ever fails, no composition that wires a kill will start.
func TestARealBrakeProvesItHalts(t *testing.T) {
	h := NewHook(Limits{MaxActions: 600, Window: time.Minute}, nil)
	if err := h.ProveHalts(); err != nil {
		t.Fatalf("ProveHalts: %v", err)
	}
}

// TestProvingLeavesNoTraceOfTheProbeRun: the proof dispatches against a run that does not
// exist, and the bookkeeping it creates does not outlive the call. A brake that accumulated
// a probe's state per composition would be leaking on the path that exists to be trusted.
func TestProvingLeavesNoTraceOfTheProbeRun(t *testing.T) {
	h := NewHook(Limits{MaxActions: 600, Window: time.Minute}, nil)
	if err := h.ProveHalts(); err != nil {
		t.Fatalf("ProveHalts: %v", err)
	}
	h.mu.Lock()
	_, kept := h.state[proofRun]
	h.mu.Unlock()
	if kept {
		t.Fatal("the proof left its probe run's breaker state behind")
	}
}

// TestEngageHaltsThroughTheHookItself: the brake is the handle, so a caller holding one can
// stop a run without reaching through it for the switch. This is what makes a *Hook usable
// as the reconciler's halt port.
func TestEngageHaltsThroughTheHookItself(t *testing.T) {
	h := NewHook(Limits{}, nil)
	h.Engage("r1", "the operator said so")

	err := h.Before(Into(context.Background(), "r1"), dispatch.Action{Name: "shell"})
	if err == nil {
		t.Fatal("an action on a halted run was admitted")
	}
	if !strings.Contains(err.Error(), "the operator said so") {
		t.Fatalf("refusal = %q, want the reason in it", err)
	}
	if err := h.Before(Into(context.Background(), "r2"), dispatch.Action{Name: "shell"}); err != nil {
		t.Fatalf("halting r1 also refused r2: %v", err)
	}
}

// --- deliberately broken halts ----------------------------------------------

// brokenSwitch is a halt state that misbehaves in one chosen way, which is how each leg of
// the proof is shown to be the leg that catches it.
type brokenSwitch struct {
	neverEngaged bool   // Engaged always says no: the silent-kill-switch failure itself
	alwaysHalted bool   // Engaged always says yes, halting runs nobody stopped
	stickyHalt   bool   // Reset does nothing, so a halted run can never come back
	reason       string // what Engaged reports, when it reports a halt at all

	engaged bool
}

func (s *brokenSwitch) Engage(_, reason string) {
	s.engaged = true
	if s.reason == "" {
		s.reason = reason
	}
}

func (s *brokenSwitch) Engaged(string) (bool, string) {
	switch {
	case s.alwaysHalted:
		return true, s.reason
	case s.neverEngaged:
		return false, ""
	default:
		return s.engaged, s.reason
	}
}

func (s *brokenSwitch) Reset(string) {
	if !s.stickyHalt {
		s.engaged = false
	}
}

var _ Switch = (*brokenSwitch)(nil)

// TestAHaltThatEngagesNothingIsCaught is the failure the proof was written for: the switch
// records the halt, reports itself engaged to anyone who asks it directly, and the waist
// lets the run keep dispatching. Every run under it looks braked and none of them is.
func TestAHaltThatEngagesNothingIsCaught(t *testing.T) {
	h := NewHook(Limits{}, &brokenSwitch{neverEngaged: true})
	err := h.ProveHalts()
	if !errors.Is(err, ErrBrakesBroken) {
		t.Fatalf("ProveHalts = %v, want %v", err, ErrBrakesBroken)
	}
	if !strings.Contains(err.Error(), "admitted") {
		t.Fatalf("error = %q, want it to name the admitted action", err)
	}
}

// TestAHaltThatRefusesEveryRunIsCaught: the opposite break, and the reason the proof checks
// the unhalted case first. A Before that refuses unconditionally satisfies "a halted run is
// refused" while halting every run there is.
func TestAHaltThatRefusesEveryRunIsCaught(t *testing.T) {
	h := NewHook(Limits{}, &brokenSwitch{alwaysHalted: true, reason: "always"})
	err := h.ProveHalts()
	if !errors.Is(err, ErrBrakesBroken) {
		t.Fatalf("ProveHalts = %v, want %v", err, ErrBrakesBroken)
	}
	if !strings.Contains(err.Error(), "no halt engaged") {
		t.Fatalf("error = %q, want it to name the run that was refused for nothing", err)
	}
}

// TestAHaltNobodyCanResetIsCaught: a halt is a state, not a one-way door. A brake a run can
// never come back through would make the first automatic breaker trip permanent, and the
// operator's reset would do nothing.
func TestAHaltNobodyCanResetIsCaught(t *testing.T) {
	h := NewHook(Limits{}, &brokenSwitch{stickyHalt: true})
	err := h.ProveHalts()
	if !errors.Is(err, ErrBrakesBroken) {
		t.Fatalf("ProveHalts = %v, want %v", err, ErrBrakesBroken)
	}
	if !strings.Contains(err.Error(), "reset") {
		t.Fatalf("error = %q, want it to name the reset", err)
	}
}

// TestAHaltWhoseReasonIsLostIsCaught: the operator's reason is the whole content of a kill,
// and a refusal that drops it leaves the person handed the stopped run with no account of
// who stopped it or why.
func TestAHaltWhoseReasonIsLostIsCaught(t *testing.T) {
	h := NewHook(Limits{}, &brokenSwitch{reason: "something else entirely"})
	err := h.ProveHalts()
	if !errors.Is(err, ErrBrakesBroken) {
		t.Fatalf("ProveHalts = %v, want %v", err, ErrBrakesBroken)
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Fatalf("error = %q, want it to name the lost reason", err)
	}
}

// withHaltError swaps the error a halted run's action is refused with. It is the one break
// no wrong halt state can produce, since the class is chosen by the Hook and not by the
// switch, so proving the class assertion catches it needs the refusal itself replaced.
func withHaltError(f func(reason string) error) Option {
	return func(h *Hook) { h.refuse = f }
}

// TestAHaltRefusedAsTransientIsCaught is the assertion easiest to leave out and the one a
// caller depends on most: a halt reported as retryable is a halt the caller retries past,
// so it delays a run instead of stopping it.
func TestAHaltRefusedAsTransientIsCaught(t *testing.T) {
	h := NewHook(Limits{}, nil, withHaltError(func(reason string) error {
		return fault.New(fault.Transient, "run_halted", "run halted by safety brake: "+reason)
	}))
	err := h.ProveHalts()
	if !errors.Is(err, ErrBrakesBroken) {
		t.Fatalf("ProveHalts = %v, want %v", err, ErrBrakesBroken)
	}
	if !strings.Contains(err.Error(), string(fault.Transient)) {
		t.Fatalf("error = %q, want it to name the class a caller would retry past", err)
	}
}
