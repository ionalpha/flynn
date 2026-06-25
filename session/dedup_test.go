package session

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

// TestTurnStartedDedupedAcrossRetries pins the fix for the "...turn 1" spam: a step
// that fails and is retried re-runs from the same turn and re-announces it, but the
// session must record each turn.started exactly once, at write time, so the durable
// stream a replay reads is clean and the turn counter never resets.
func TestTurnStartedDedupedAcrossRetries(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	s := New(log, bus.NewMemory(), WithID("run-1"))
	rep := s.Reporter()

	// Turn 1 announced, then retried twice (re-announced), then it produces text and
	// the run advances to turn 2, which is itself retried once.
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Turn: 1})
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Turn: 1})
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Turn: 1})
	rep.Report(ctx, mission.Event{Kind: mission.EventAssistantText, Turn: 1, Text: "hi"})
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Turn: 2})
	rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Turn: 2})

	events, err := History(ctx, log, "run-1")
	if err != nil {
		t.Fatal(err)
	}

	var startedTurns []int
	for _, ev := range events {
		if ev.Kind == KindTurnStarted {
			startedTurns = append(startedTurns, ev.Turn)
		}
	}
	if len(startedTurns) != 2 || startedTurns[0] != 1 || startedTurns[1] != 2 {
		t.Fatalf("turn.started events = %v, want exactly [1 2] (one per turn, retries deduped)", startedTurns)
	}
}

// TestTurnStartedDedupProperty pins the invariant over arbitrary announce patterns:
// for any non-decreasing sequence of announced turns (a turn re-announced any number
// of times by retries), the stream records each distinct turn's start exactly once,
// in increasing order.
func TestTurnStartedDedupProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Build a non-decreasing sequence of turn numbers with arbitrary repeats.
		steps := rapid.IntRange(1, 25).Draw(rt, "steps")
		turn := 1
		announced := make([]int, 0, steps)
		for range steps {
			if rapid.Bool().Draw(rt, "advance") {
				turn++
			}
			announced = append(announced, turn)
		}

		ctx := context.Background()
		log := spine.NewMemoryLog()
		s := New(log, bus.NewMemory(), WithID("r"))
		rep := s.Reporter()
		for _, n := range announced {
			rep.Report(ctx, mission.Event{Kind: mission.EventTurnStarted, Turn: n})
		}

		events, err := History(ctx, log, "r")
		if err != nil {
			rt.Fatal(err)
		}
		var got []int
		for _, e := range events {
			if e.Kind == KindTurnStarted {
				got = append(got, e.Turn)
			}
		}
		// Expected: the strictly-increasing distinct values of the announced sequence.
		var want []int
		last := 0
		for _, n := range announced {
			if n > last {
				want = append(want, n)
				last = n
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			rt.Fatalf("recorded turn.started = %v, want %v (distinct, increasing, once each)", got, want)
		}
	})
}
