package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/brakes"
)

// unprovableHalt is a halt that cannot demonstrate it halts. It stands for the real thing
// this guard is aimed at: a brake wired into a composition whose enforcement path has
// stopped enforcing, which changes nothing observable until an operator tries to stop a run
// and it keeps going.
type unprovableHalt struct{ engaged int }

func (h *unprovableHalt) Engage(string, string) { h.engaged++ }

func (h *unprovableHalt) ProveHalts() error { return errors.New("the halt did not refuse") }

// TestARuntimeRefusesAHaltThatCannotProveItHalts: the proof runs at composition, so a
// broken kill switch stops the process from starting rather than being wired in and
// silently stopping nothing. It is the evidence gate's rule applied to the other gate whose
// failure mode is silence.
func TestARuntimeRefusesAHaltThatCannotProveItHalts(t *testing.T) {
	halt := &unprovableHalt{}
	_, err := New(Config{Executor: stubExec{}, Stop: stubStop{}, Halt: halt})
	if err == nil {
		t.Fatal("New built a runtime over a halt that cannot halt")
	}
	if !strings.Contains(err.Error(), "kill switch") {
		t.Fatalf("error = %q, want it to name the kill switch", err)
	}
	if halt.engaged != 0 {
		t.Fatal("the broken halt was used before it was proved")
	}
}

// TestARuntimeTakesTheShippedBrakeAsItsHalt: the brake a run's actions are dispatched
// through is the thing that gets wired as the kill, and it proves itself on the way in.
func TestARuntimeTakesTheShippedBrakeAsItsHalt(t *testing.T) {
	brk := brakes.NewHook(brakes.Limits{MaxActions: 600, Window: time.Minute}, nil)
	if _, err := New(Config{Executor: stubExec{}, Stop: stubStop{}, Halt: brk}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// TestARuntimeWithNoHaltStillBuilds: an unwired halt is a supported composition, not a
// broken one. A kill still settles the goal; it just waits for the running step.
func TestARuntimeWithNoHaltStillBuilds(t *testing.T) {
	if _, err := New(Config{Executor: stubExec{}, Stop: stubStop{}}); err != nil {
		t.Fatalf("New: %v", err)
	}
}
