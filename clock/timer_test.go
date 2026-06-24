package clock

import (
	"testing"
	"time"
)

func epoch() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func fired(t Timer) bool {
	select {
	case <-t.C():
		return true
	default:
		return false
	}
}

func TestManualTimerFiresOnAdvance(t *testing.T) {
	m := NewManual(epoch())
	tm := m.NewTimer(10 * time.Second)

	if fired(tm) {
		t.Fatal("timer fired before its deadline")
	}
	m.Advance(9 * time.Second)
	if fired(tm) {
		t.Fatal("timer fired before deadline (9s < 10s)")
	}
	m.Advance(1 * time.Second) // now at deadline
	if !fired(tm) {
		t.Fatal("timer did not fire at its deadline")
	}
	// Single-shot: it does not fire again.
	if fired(tm) {
		t.Fatal("timer fired twice")
	}
}

func TestManualTimerImmediate(t *testing.T) {
	m := NewManual(epoch())
	if !fired(m.NewTimer(0)) {
		t.Fatal("NewTimer(0) should fire immediately")
	}
	if !fired(m.NewTimer(-1)) {
		t.Fatal("NewTimer(negative) should fire immediately")
	}
}

func TestManualTimerStop(t *testing.T) {
	m := NewManual(epoch())
	tm := m.NewTimer(5 * time.Second)
	if !tm.Stop() {
		t.Fatal("Stop of a pending timer should report true")
	}
	m.Advance(10 * time.Second)
	if fired(tm) {
		t.Fatal("stopped timer must not fire")
	}
	if tm.Stop() {
		t.Fatal("second Stop should report false (already stopped)")
	}
}

func TestManualTimerReset(t *testing.T) {
	m := NewManual(epoch())
	tm := m.NewTimer(5 * time.Second)
	m.Advance(3 * time.Second)
	if !tm.Reset(5 * time.Second) { // pending before reset -> true
		t.Fatal("Reset of a pending timer should report true")
	}
	m.Advance(4 * time.Second) // 4s < new 5s deadline
	if fired(tm) {
		t.Fatal("reset timer fired before its new deadline")
	}
	m.Advance(1 * time.Second) // reaches new deadline
	if !fired(tm) {
		t.Fatal("reset timer did not fire at its new deadline")
	}
}

func TestManualAfter(t *testing.T) {
	m := NewManual(epoch())
	ch := m.After(2 * time.Second)
	select {
	case <-ch:
		t.Fatal("After channel fired early")
	default:
	}
	m.Advance(2 * time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("After channel did not fire on advance")
	}
}

func TestSystemTimerReal(t *testing.T) {
	var s System
	tm := s.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(time.Second):
		t.Fatal("system timer did not fire")
	}
}
