package procs

import (
	"testing"

	"pgregory.net/rapid"
)

// TestLiveEqualsUnreapedSpawns is the registry's invariant under any interleaving of
// spawns and reaps, including reaps repeated on children already reaped: Live is exactly
// the number of children started whose reap function has been called at least zero times
// and never once, and it is never negative.
//
// Negativity is the failure that matters. The count feeds a leak detector, and a registry
// that can be driven below zero lets a real unreaped child hide behind a stale
// double-decrement from some unrelated error path.
func TestLiveEqualsUnreapedSpawns(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var r Registry

		// reaps holds the reap function of every child started and not yet reaped; the
		// model's Live is simply len(reaps).
		var reaps []func()

		ops := rapid.SliceOfN(rapid.IntRange(0, 2), 1, 64).Draw(t, "ops")
		for _, op := range ops {
			switch op {
			case 0: // start a child
				reaps = append(reaps, r.Started())

			case 1: // reap a live child, if any
				if len(reaps) == 0 {
					break
				}
				i := rapid.IntRange(0, len(reaps)-1).Draw(t, "reap")
				reaps[i]()
				reaps = append(reaps[:i], reaps[i+1:]...)

			case 2: // reap an already-reaped child: must not decrement again
				if len(reaps) == 0 {
					break
				}
				i := rapid.IntRange(0, len(reaps)-1).Draw(t, "double")
				reaps[i]()
				reaps[i]()
				reaps[i]()
				reaps = append(reaps[:i], reaps[i+1:]...)
			}

			if got, want := r.Live(), len(reaps); got != want {
				t.Fatalf("Live is %d, want %d unreaped children", got, want)
			}
			if r.Live() < 0 {
				t.Fatalf("Live went negative: %d", r.Live())
			}
		}

		// Reaping everything that is left returns the registry to zero, whatever the path
		// taken to get here. A spawner that shuts down cleanly leaves no phantom children.
		for _, reap := range reaps {
			reap()
		}
		if got := r.Live(); got != 0 {
			t.Fatalf("after reaping every child: Live is %d, want 0", got)
		}
	})
}
