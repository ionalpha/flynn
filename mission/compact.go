package mission

import (
	"fmt"

	"github.com/ionalpha/flynn/llm"
)

// minCompactMessages is the shortest transcript worth compacting. Below it there is
// no middle to elide (an objective, at most one exchange, and the live tail), so
// compaction would either do nothing or eat the recent context.
const minCompactMessages = 6

// charsPerToken is the rough bytes-per-token ratio used to estimate a transcript's
// size without a tokenizer. It is deliberately conservative (real tokenizers average
// near four bytes per token for prose and less for code), so the estimate over- not
// under-counts and compaction triggers a little early rather than too late. Exact
// per-model accounting arrives with the model registry.
const charsPerToken = 4

// compactView returns a view of a transcript that fits within a token budget by
// eliding the oldest middle turns, when it would otherwise overflow. The objective
// (the first message) is always kept as the head, the most recent turns are kept as
// the tail, and the gap between them is replaced by a short note folded into the
// head. It is the coarse fallback beneath result pruning: pruning trims large
// outputs turn by turn, and this caps the total when even a pruned transcript grows
// past the budget.
//
// Like pruning, it never mutates msgs: the durable checkpoint keeps every turn, and
// this is a transient view, so nothing is lost (the README's guarantee that
// compaction is a view over the log, not an overwrite). A budget of zero disables
// it. It is deterministic (a pure function of the transcript and the budget), so a
// replay reproduces the same view.
//
// The cut is chosen so the kept tail begins at an assistant turn. The mission loop
// builds a strictly alternating user, assistant, user transcript, so cutting there
// keeps roles alternating and keeps every tool call together with the result that
// answers it, never orphaning one across the elision.
func compactView(msgs []llm.Message, budgetTokens int) []llm.Message {
	if budgetTokens <= 0 || len(msgs) < minCompactMessages {
		return msgs
	}
	if estimateTokens(msgs) <= budgetTokens {
		return msgs
	}

	// Grow the tail from the end until it fills its share of the budget, leaving room
	// for the head and the model's reply.
	tailBudget := budgetTokens / 2
	cut, used := len(msgs), 0
	for i := len(msgs) - 1; i >= 1; i-- {
		t := messageTokens(msgs[i])
		if used+t > tailBudget && cut < len(msgs) {
			break
		}
		used += t
		cut = i
	}

	// Snap the cut to an assistant turn so the view still alternates user, assistant
	// and no tool result is separated from its call.
	if msgs[cut].Role != llm.RoleAssistant {
		cut++
	}
	if cut <= 1 || cut >= len(msgs) {
		return msgs // nothing meaningful to elide, or the tail is already everything
	}

	out := make([]llm.Message, 0, 1+(len(msgs)-cut))
	out = append(out, withElisionNote(msgs[0], cut-1))
	out = append(out, msgs[cut:]...)
	return out
}

// withElisionNote returns a copy of the head message with a short note that n turns
// were elided, so the model knows the history was trimmed (and is recoverable on the
// log) rather than silently shortened. The original message is not modified.
func withElisionNote(head llm.Message, n int) llm.Message {
	blocks := append([]llm.Block(nil), head.Blocks...)
	note := fmt.Sprintf("[%d earlier turns were elided here to stay within the context budget. The full history is preserved on the run log and can be replayed.]", n)
	blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: note})
	return llm.Message{Role: head.Role, Blocks: blocks}
}

// estimateTokens approximates the token cost of a whole transcript.
func estimateTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += messageTokens(m)
	}
	return n
}

// messageTokens approximates one message's token cost from the size of its content.
// It counts text, tool arguments, and tool results, the parts that actually grow,
// and adds a small per-block overhead for structural tokens.
func messageTokens(m llm.Message) int {
	chars := 0
	for _, b := range m.Blocks {
		chars += blockSize(b)
	}
	return chars/charsPerToken + len(m.Blocks)*4
}

// blockSize is the byte size of a block's payload, used by the token estimate.
func blockSize(b llm.Block) int {
	switch b.Kind {
	case llm.KindText:
		return len(b.Text)
	case llm.KindToolUse:
		if b.ToolUse != nil {
			return len(b.ToolUse.Name) + len(b.ToolUse.Input)
		}
	case llm.KindToolResult:
		if b.ToolResult != nil {
			return len(b.ToolResult.Content)
		}
	case llm.KindOpaque:
		return len(b.Raw)
	}
	return 0
}
