//go:build !race

// Allocation ceilings for the reconcile read path. A settled goal is reconciled again
// on every resync, and its status embeds the whole transcript in the opaque Checkpoint,
// so the pre-skip decode must not scale with the transcript. Excluded under -race
// (instrumentation skews allocation counts); dev/bench and the CI bench job run them.

package goal

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// settledStatusOfDepth builds a settled goal's status carrying a Checkpoint of the given
// transcript depth, the shape a long-running goal's resync reads.
func settledStatusOfDepth(t *testing.T, depth int) resource.Resource {
	t.Helper()
	msgs := make([]map[string]string, depth)
	for i := range msgs {
		msgs[i] = map[string]string{"role": "assistant", "content": fmt.Sprintf("turn %d: a representative message body", i)}
	}
	ckpt, err := json.Marshal(map[string]any{"messages": msgs, "steps": depth})
	if err != nil {
		t.Fatal(err)
	}
	st, err := Status{Phase: PhaseConverged, ObservedSpecHash: "abc", Steps: depth, Checkpoint: ckpt}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{Status: st}
}

func headAllocsAtDepth(t *testing.T, depth int) float64 {
	t.Helper()
	r := settledStatusOfDepth(t, depth)
	return testing.AllocsPerRun(50, func() {
		if _, err := decodeStatusHead(r); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAllocCeilingStatusHeadFlatInCheckpointSize pins the settled-goal resync guarantee:
// the pre-skip status decode reads only the phase and observed spec hash, so it costs the
// same whether the goal's Checkpoint transcript is depth n or 2n. The full DecodeStatus it
// replaced copied the whole Checkpoint into a json.RawMessage on every pass.
func TestAllocCeilingStatusHeadFlatInCheckpointSize(t *testing.T) {
	n := headAllocsAtDepth(t, 200)
	n2 := headAllocsAtDepth(t, 400)
	if n != n2 {
		t.Errorf("decodeStatusHead allocates %.0f/op at transcript depth 200 but %.0f/op at 400: the pre-skip decode is scaling with the Checkpoint size", n, n2)
	}
}

// TestStatusHeadDoesNotCopyCheckpoint is the counterexample: the full DecodeStatus does
// scale with the Checkpoint (it copies it into a json.RawMessage), so the head decode
// allocating strictly less at depth proves it is not paying that copy.
func TestStatusHeadDoesNotCopyCheckpoint(t *testing.T) {
	r := settledStatusOfDepth(t, 400)
	head := testing.AllocsPerRun(50, func() {
		if _, err := decodeStatusHead(r); err != nil {
			t.Fatal(err)
		}
	})
	full := testing.AllocsPerRun(50, func() {
		if _, err := DecodeStatus(r); err != nil {
			t.Fatal(err)
		}
	})
	if head >= full {
		t.Errorf("decodeStatusHead allocates %.0f/op, not less than the full DecodeStatus at %.0f/op: it is not skipping the Checkpoint copy", head, full)
	}
}
