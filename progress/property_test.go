package progress

import (
	"context"
	"encoding/json"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// Property: the probe's fingerprint depends only on the SET of distinct tool actions,
// not on how many times each was repeated. This is the idle property that makes
// no-progress detection work — re-running work a run has already done adds nothing, so a
// loop of repeats reads as no progress — expressed over random inputs rather than a
// handful of fixtures. A stream of calls with arbitrary duplication fingerprints the same
// as the distinct set of those calls appended once.
func TestProp_FingerprintIgnoresDuplicateActions(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		toolGen := rapid.SampledFrom([]string{"read", "write", "bash", "grep"})
		argGen := rapid.StringMatching(`[a-c]{1,3}`)
		type call struct {
			tool, arg string
			dups      int
		}
		callGen := rapid.Custom(func(t *rapid.T) call {
			return call{
				tool: toolGen.Draw(t, "tool"),
				arg:  argGen.Draw(t, "arg"),
				dups: rapid.IntRange(0, 3).Draw(t, "dups"),
			}
		})
		calls := rapid.SliceOfN(callGen, 0, 12).Draw(rt, "calls")

		// Stream A: every call, each repeated an arbitrary number of extra times.
		logA := spine.NewMemoryLog()
		for _, c := range calls {
			for range c.dups + 1 {
				appendCallOn(rt, logA, "A", c.tool, c.arg)
			}
		}

		// Stream B: only the distinct (tool, arg) pairs, once each.
		logB := spine.NewMemoryLog()
		seen := map[call]bool{}
		for _, c := range calls {
			key := call{tool: c.tool, arg: c.arg}
			if seen[key] {
				continue
			}
			seen[key] = true
			appendCallOn(rt, logB, "B", c.tool, c.arg)
		}

		fpA := drawFingerprint(rt, probe(logA, ""), "A")
		fpB := drawFingerprint(rt, probe(logB, ""), "B")
		if fpA != fpB {
			rt.Fatalf("duplication changed the fingerprint: dup=%q distinct=%q", fpA, fpB)
		}
	})
}

// appendCallOn records a tool.call on the named stream in the session wire format.
func appendCallOn(rt *rapid.T, log spine.Log, streamName, tool, arg string) {
	body, err := json.Marshal(map[string]any{
		"kind":  "tool.call",
		"tool":  tool,
		"input": json.RawMessage(`{"path":"` + arg + `"}`),
	})
	if err != nil {
		rt.Fatal(err)
	}
	if _, err := log.Append(context.Background(), spine.AppendInput{
		Stream:  streamName,
		Type:    "tool.call",
		Payload: map[string]any{"event": string(body)},
	}); err != nil {
		rt.Fatalf("append: %v", err)
	}
}

func drawFingerprint(rt *rapid.T, p *SpineProbe, streamName string) string {
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: streamName}
	fp, _, err := p.Progress(context.Background(), r)
	if err != nil {
		rt.Fatalf("progress: %v", err)
	}
	return fp
}
