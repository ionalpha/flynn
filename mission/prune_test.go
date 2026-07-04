package mission

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm"
)

// fakeSummarizer is a tool stand-in that returns a fixed one-line summary, so a test
// can assert the summarizer path without a real tool.
type fakeSummarizer struct{ line string }

func (f fakeSummarizer) SummarizeResult(json.RawMessage, string) string { return f.line }

// big returns a result body comfortably over the prune threshold.
func big(seed string) string { return seed + strings.Repeat("x", pruneResultThreshold+1) }

// call builds an assistant tool_use message; result builds the user message that
// answers it. Kept tiny so tests read as transcripts.
func callMsg(id, name string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(`{}`)}},
	}}
}

func resultMsg(id, content string, isErr bool) llm.Message {
	return llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
		{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{ToolUseID: id, Content: content, IsError: isErr}},
	}}
}

func noSummarizer(string) ResultSummarizer { return nil }

// firstResult returns the content of the first tool-result block in msgs.
func firstResult(msgs []llm.Message) string {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult && b.ToolResult != nil {
				return b.ToolResult.Content
			}
		}
	}
	return ""
}

func TestPruneKeepsRecentSmallAndErrors(t *testing.T) {
	bigA, bigB := big("a"), big("b")
	msgs := []llm.Message{
		callMsg("1", "bash"), resultMsg("1", bigA, false), // older large bash -> pruned
		callMsg("2", "read"), resultMsg("2", "tiny", false), // small -> kept
		callMsg("3", "edit"), resultMsg("3", big("err"), true), // error -> kept even if large
		callMsg("4", "bash"), resultMsg("4", bigB, false), // most recent bash -> kept
	}
	out := pruneTranscript(msgs, func(name string) ResultSummarizer {
		if name == "bash" {
			return fakeSummarizer{"ran it"}
		}
		return nil
	})

	if got := out[1].Blocks[0].ToolResult.Content; !strings.Contains(got, "[pruned bash: ran it]") {
		t.Fatalf("older bash result should be summarized, got %q", got)
	}
	if out[3].Blocks[0].ToolResult.Content != "tiny" {
		t.Fatal("small result must be kept verbatim")
	}
	if out[5].Blocks[0].ToolResult.Content != big("err") {
		t.Fatal("error result must be kept verbatim even when large")
	}
	if out[7].Blocks[0].ToolResult.Content != bigB {
		t.Fatal("most recent bash result must be kept verbatim")
	}
	// The input must be untouched: the checkpoint stays the lossless source of truth.
	if msgs[1].Blocks[0].ToolResult.Content != bigA {
		t.Fatal("pruneTranscript mutated its input")
	}
}

func TestPruneDedupesIdenticalResults(t *testing.T) {
	same := big("dup")
	msgs := []llm.Message{
		callMsg("1", "bash"), resultMsg("1", same, false), // older bash, identical -> duplicate note
		callMsg("2", "bash"), resultMsg("2", same, false), // latest bash, identical -> kept verbatim
	}
	out := pruneTranscript(msgs, noSummarizer)
	if got := out[1].Blocks[0].ToolResult.Content; !strings.Contains(got, "identical to a later result") {
		t.Fatalf("earlier identical result should be deduped, got %q", got)
	}
	if out[3].Blocks[0].ToolResult.Content != same {
		t.Fatal("the most recent identical result must be kept verbatim")
	}
}

func TestPruneGenericFallback(t *testing.T) {
	msgs := []llm.Message{
		callMsg("1", "mystery"), resultMsg("1", big("x"), false),
		callMsg("2", "mystery"), resultMsg("2", "small", false),
	}
	out := pruneTranscript(msgs, noSummarizer)
	got := firstResult(out)
	if !strings.Contains(got, "[pruned mystery:") || !strings.Contains(got, "chars]") {
		t.Fatalf("generic summary expected, got %q", got)
	}
}

// TestPruneProperties is the safety net: over random transcripts, pruning preserves
// structure (message and block counts, tool-call pairing), never elides a small
// result or an error, is idempotent, and never mutates its input.
func TestPruneProperties(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nPairs := rapid.IntRange(0, 8).Draw(rt, "pairs")
		var msgs []llm.Message
		for i := range nPairs {
			id := rapid.StringMatching(`[a-z0-9]{1,4}`).Draw(rt, "id")
			name := rapid.SampledFrom([]string{"bash", "read", "grep", "other", ""}).Draw(rt, "name")
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(`{}`)}},
			}})
			var content string
			switch rapid.IntRange(0, 2).Draw(rt, "size") {
			case 0:
				content = rapid.StringMatching(`[a-z]{0,20}`).Draw(rt, "small")
			default:
				content = big(rapid.StringMatching(`[a-z]{0,4}`).Draw(rt, "bigseed"))
			}
			isErr := rapid.Bool().Draw(rt, "err")
			msgs = append(msgs, resultMsg(id, content, isErr))
			_ = i
		}

		// A deep copy to detect any mutation of the input.
		before := deepCopyMessages(msgs)
		sum := func(name string) ResultSummarizer {
			if name == "bash" {
				return fakeSummarizer{"summary"}
			}
			return nil
		}
		out := pruneTranscript(msgs, sum)

		if !reflect.DeepEqual(msgs, before) {
			rt.Fatal("pruneTranscript mutated its input")
		}
		if len(out) != len(msgs) {
			rt.Fatalf("message count changed: %d -> %d", len(msgs), len(out))
		}
		for i := range out {
			if len(out[i].Blocks) != len(msgs[i].Blocks) || out[i].Role != msgs[i].Role {
				rt.Fatalf("message %d shape changed", i)
			}
			for j, b := range out[i].Blocks {
				orig := msgs[i].Blocks[j]
				if b.Kind != orig.Kind {
					rt.Fatalf("block %d/%d kind changed", i, j)
				}
				if b.Kind == llm.KindToolResult {
					// Pairing and error flag are preserved; only content may change.
					if b.ToolResult.ToolUseID != orig.ToolResult.ToolUseID || b.ToolResult.IsError != orig.ToolResult.IsError {
						rt.Fatalf("tool-result pairing/error flag changed at %d/%d", i, j)
					}
					small := len(orig.ToolResult.Content) <= pruneResultThreshold
					if (orig.ToolResult.IsError || small) && b.ToolResult.Content != orig.ToolResult.Content {
						rt.Fatalf("an error or small result was elided at %d/%d", i, j)
					}
				}
			}
		}

		// Idempotence: a pruned transcript prunes to itself (summaries are below the
		// threshold and no longer duplicate the verbatim originals).
		twice := pruneTranscript(out, sum)
		if !reflect.DeepEqual(out, twice) {
			rt.Fatal("pruneTranscript is not idempotent")
		}
	})
}

// TestHashStringMatchesFNV pins hashString to FNV-1a: it must stay byte-identical to
// hash/fnv so a transcript prunes the same way on replay and across builds, while
// avoiding the []byte copy. A drift here would make duplicate detection process- or
// version-dependent.
func TestHashStringMatchesFNV(t *testing.T) {
	cases := []string{"", "a", "hello world", big("seed"), strings.Repeat("ab", 5000), "\x00\xff\x80\n"}
	for _, s := range cases {
		h := fnv.New64a()
		_, _ = h.Write([]byte(s))
		if got, want := hashString(s), h.Sum64(); got != want {
			t.Fatalf("hashString(%d bytes) = %d, want fnv %d", len(s), got, want)
		}
	}
}

// TestPruneSkipsWhenNothingLarge proves the fast path is behavior-preserving: a
// transcript whose results are all small or errors has nothing prunable, so the view
// is the input unchanged. This is the same result the full pass produces, so skipping
// it cannot alter the transcript sent to the model.
func TestPruneSkipsWhenNothingLarge(t *testing.T) {
	msgs := []llm.Message{
		callMsg("1", "read"), resultMsg("1", "small", false),
		callMsg("2", "bash"), resultMsg("2", big("err"), true), // large but an error -> never pruned
		callMsg("3", "grep"), resultMsg("3", "also small", false),
	}
	out := pruneTranscript(msgs, noSummarizer)
	if !reflect.DeepEqual(out, msgs) {
		t.Fatal("a transcript with nothing prunable must be returned unchanged")
	}
}

// TestPruneCommonCaseZeroAlloc is the M12 allocation-ceiling gate: pruning a
// transcript whose results are all small (the common turn) must not allocate, and
// that must hold as the transcript grows, so the per-turn pass stays flat instead of
// adding O(history) allocation to every turn over a goal. The large-result summarizing
// path is measured separately by BenchmarkPruneTranscript.
func TestPruneCommonCaseZeroAlloc(t *testing.T) {
	build := func(pairs int) []llm.Message {
		msgs := make([]llm.Message, 0, pairs*2)
		for i := range pairs {
			id := fmt.Sprintf("t%d", i)
			msgs = append(msgs, callMsg(id, "read"), resultMsg(id, "small result", false))
		}
		return msgs
	}
	for _, pairs := range []int{50, 100} {
		msgs := build(pairs)
		allocs := testing.AllocsPerRun(200, func() {
			_ = pruneTranscript(msgs, noSummarizer)
		})
		if allocs != 0 {
			t.Fatalf("pruneTranscript over %d small-result pairs allocated %.0f times; the no-large-result turn must be allocation-free and stay flat as history grows", pairs, allocs)
		}
	}
}

// deepCopyMessages clones a transcript so a test can detect in-place mutation. A nil
// input copies to nil (not an empty slice), so a reflect.DeepEqual against the
// original is exact for the zero-pair case too.
func deepCopyMessages(msgs []llm.Message) []llm.Message {
	if msgs == nil {
		return nil
	}
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		out[i] = llm.Message{Role: m.Role, Blocks: append([]llm.Block(nil), m.Blocks...)}
		for j, b := range m.Blocks {
			if b.ToolResult != nil {
				r := *b.ToolResult
				out[i].Blocks[j].ToolResult = &r
			}
			if b.ToolUse != nil {
				u := *b.ToolUse
				out[i].Blocks[j].ToolUse = &u
			}
		}
	}
	return out
}
