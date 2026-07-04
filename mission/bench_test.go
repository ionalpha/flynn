package mission

import (
	"context"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
)

// benchTranscript builds a transcript of `pairs` tool call/result exchanges. With
// large=true every result is over the prune threshold (the summarizing path); with
// large=false results are small (the common no-op path).
func benchTranscript(pairs int, large bool) []llm.Message {
	msgs := make([]llm.Message, 0, pairs*2)
	for i := range pairs {
		id := fmt.Sprintf("t%d", i)
		content := "small result"
		if large {
			content = big(fmt.Sprintf("s%d", i))
		}
		msgs = append(msgs, callMsg(id, "read"), resultMsg(id, content, false))
	}
	return msgs
}

// BenchmarkPruneTranscript measures the per-turn prune at two history sizes (n and
// 2n), for both the common all-small-results turn and the large-result summarizing
// turn. Cost should scale about linearly with history, not quadratically, and the
// small-result case should report zero allocs.
func BenchmarkPruneTranscript(b *testing.B) {
	for _, large := range []bool{false, true} {
		name := "small"
		if large {
			name = "large"
		}
		for _, pairs := range []int{64, 128} {
			msgs := benchTranscript(pairs, large)
			b.Run(fmt.Sprintf("%s/%d", name, pairs), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					_ = pruneTranscript(msgs, noSummarizer)
				}
			})
		}
	}
}

// BenchmarkConvergenceMet measures the convergence check at two history sizes. It
// decodes only the outcome fields, so allocs/op should stay flat as the transcript
// grows rather than rebuilding the whole message history each tick.
func BenchmarkConvergenceMet(b *testing.B) {
	for _, pairs := range []int{64, 128} {
		raw, err := encodeCheckpoint(checkpoint{Messages: benchTranscript(pairs, true), Done: true, Result: "done"})
		if err != nil {
			b.Fatal(err)
		}
		status := goal.Status{Checkpoint: raw}
		b.Run(fmt.Sprintf("pairs=%d", pairs), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				ok, _, err := (Convergence{}).Met(context.Background(), goal.Spec{}, status)
				if err != nil || !ok {
					b.Fatalf("Met = %v, err = %v", ok, err)
				}
			}
		})
	}
}
