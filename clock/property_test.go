package clock_test

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
)

// start is an arbitrary fixed origin for the Manual clock in these properties.
var start = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// Property: after any sequence of non-negative Advances, Now is exactly the
// origin plus the sum of the advances - the Manual clock neither drifts nor
// moves on its own.
func TestProp_ManualAdvanceAccumulates(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := clock.NewManual(start)
		total := time.Duration(0)
		for _, ms := range rapid.SliceOfN(rapid.Int64Range(0, 1_000_000), 0, 20).Draw(rt, "steps") {
			d := time.Duration(ms) * time.Microsecond
			m.Advance(d)
			total += d
		}
		if got, want := m.Now(), start.Add(total); !got.Equal(want) {
			rt.Fatalf("Now() = %v, want %v", got, want)
		}
	})
}

// Property: a timer has fired exactly when the clock has reached its deadline,
// however the total advance is split into steps, and PendingTimers always equals
// the number of timers still waiting.
func TestProp_TimersFireIffDeadlineReached(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := clock.NewManual(start)

		durs := rapid.SliceOfN(rapid.Int64Range(1, 1000), 1, 8).Draw(rt, "durations")
		timers := make([]clock.Timer, len(durs))
		for i, d := range durs {
			timers[i] = m.NewTimer(time.Duration(d) * time.Millisecond)
		}

		elapsed := int64(0)
		for _, step := range rapid.SliceOfN(rapid.Int64Range(0, 400), 0, 10).Draw(rt, "steps") {
			m.Advance(time.Duration(step) * time.Millisecond)
			elapsed += step
		}

		pending := 0
		for i, tm := range timers {
			fired := false
			select {
			case got := <-tm.C():
				fired = true
				// A timer delivers the clock's time at the moment it fired, never a
				// time before its own deadline.
				if got.Before(start.Add(time.Duration(durs[i]) * time.Millisecond)) {
					rt.Fatalf("timer %d fired at %v, before its deadline", i, got)
				}
			default:
			}
			want := durs[i] <= elapsed
			if fired != want {
				rt.Fatalf("timer %d (dur %dms, elapsed %dms): fired = %v, want %v", i, durs[i], elapsed, want, fired)
			}
			if !fired {
				pending++
			}
		}
		if got := m.PendingTimers(); got != pending {
			rt.Fatalf("PendingTimers() = %d, want %d", got, pending)
		}
	})
}

// Property: Stop before the deadline reports true and the timer never fires, no
// matter how far the clock then advances; a second Stop, or a Stop after firing,
// reports false. This matches time.Timer.Stop.
func TestProp_StopPreventsFire(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := clock.NewManual(start)
		d := time.Duration(rapid.Int64Range(1, 1000).Draw(rt, "dur")) * time.Millisecond
		tm := m.NewTimer(d)

		if rapid.Bool().Draw(rt, "stopEarly") {
			if !tm.Stop() {
				rt.Fatal("Stop() on a pending timer = false, want true")
			}
			m.Advance(d * 2)
			select {
			case <-tm.C():
				rt.Fatal("stopped timer fired")
			default:
			}
			if tm.Stop() {
				rt.Fatal("second Stop() = true, want false")
			}
		} else {
			m.Advance(d)
			if tm.Stop() {
				rt.Fatal("Stop() after firing = true, want false")
			}
			select {
			case <-tm.C():
			default:
				rt.Fatal("timer at its deadline did not fire")
			}
		}
	})
}

// Property: Reset re-arms a timer for a fresh deadline from the current time,
// draining any stale fire, so it fires exactly once more when the new deadline
// is reached - whether the reset happened before or after the first fire.
func TestProp_ResetRearmsFromNow(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := clock.NewManual(start)
		first := time.Duration(rapid.Int64Range(1, 500).Draw(rt, "first")) * time.Millisecond
		second := time.Duration(rapid.Int64Range(1, 500).Draw(rt, "second")) * time.Millisecond
		fireFirst := rapid.Bool().Draw(rt, "fireFirst")

		tm := m.NewTimer(first)
		if fireFirst {
			m.Advance(first)
		}
		wasPending := tm.Reset(second)
		if wasPending == fireFirst {
			rt.Fatalf("Reset() = %v with fireFirst=%v, want the opposite", wasPending, fireFirst)
		}

		// Just short of the new deadline: nothing pending on the channel.
		m.Advance(second - time.Millisecond)
		select {
		case <-tm.C():
			rt.Fatal("reset timer fired before its new deadline")
		default:
		}

		m.Advance(time.Millisecond)
		select {
		case <-tm.C():
		default:
			rt.Fatal("reset timer did not fire at its new deadline")
		}
	})
}
