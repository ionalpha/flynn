package reconcile

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/clock"
)

// TestQueueSerializationProperty model-checks the load-bearing invariant under
// random op sequences: Get never returns a key that is currently being processed
// (so one key is never reconciled concurrently), and the ready set never exceeds
// the number of distinct keys (dedup holds). A single goroutine drives the queue,
// so Len > 0 guarantees Get does not block.
func TestQueueSerializationProperty(t *testing.T) {
	const keys = 5
	rapid.Check(t, func(rt *rapid.T) {
		q := NewQueue[int](clock.System{})
		keyGen := rapid.IntRange(0, keys-1)
		processing := map[int]bool{}

		n := rapid.IntRange(1, 300).Draw(rt, "ops")
		for i := 0; i < n; i++ {
			switch rapid.SampledFrom([]string{"add", "get", "done"}).Draw(rt, "op") {
			case "add":
				q.Add(keyGen.Draw(rt, "k"))
			case "get":
				if q.Len() == 0 {
					continue
				}
				item, shutdown := q.Get()
				if shutdown {
					rt.Fatal("unexpected shutdown")
				}
				if processing[item] {
					rt.Fatalf("key %d fetched while already processing: concurrent reconcile", item)
				}
				processing[item] = true
			case "done":
				for k := range processing {
					q.Done(k)
					delete(processing, k)
					break
				}
			}
			if q.Len() > keys {
				rt.Fatalf("ready set %d exceeds %d distinct keys: dedup broke", q.Len(), keys)
			}
		}
	})
}
