package mission

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// minCompactMessages is the shortest transcript with a middle to elide; below it compaction
// would eat the recent context.
const minCompactMessages = 6

// charsPerToken estimates size without a tokenizer. Code tokenizes below four bytes per
// token, so the estimate overcounts and compaction triggers early.
const charsPerToken = 4

// imageTokens is a flat cost near a vision model's per-image ceiling, because the estimate
// does not decode dimensions; it overcounts a small image.
const imageTokens = 1500

// compactView elides the oldest middle turns when a transcript, even pruned, passes
// budgetTokens, keeping the objective as head and noting the gap in it. A zero budget
// disables it.
//
// It never mutates msgs, because the checkpoint keeps every turn, and it is pure, so a
// replay reproduces the view. The tail starts at an assistant turn so roles still
// alternate and no tool result is parted from its call.
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

	if msgs[cut].Role != llm.RoleAssistant {
		cut++
	}
	if cut <= 1 || cut >= len(msgs) {
		return msgs // nothing meaningful to elide, or the tail is already everything
	}

	out := make([]llm.Message, 0, 1+(len(msgs)-cut))
	out = append(out, withElisionNote(msgs[0], msgs[1:cut]))
	out = append(out, msgs[cut:]...)

	// On a tiny transcript the note can cost more than the turns it replaces.
	if estimateTokens(out) >= estimateTokens(msgs) {
		return msgs
	}
	return out
}

// withElisionNote copies head and appends a digest of the elided work, so the model knows
// what happened in the gap and not only how many turns.
func withElisionNote(head llm.Message, elided []llm.Message) llm.Message {
	blocks := append([]llm.Block(nil), head.Blocks...)
	note := fmt.Sprintf("[%d earlier turns were elided here to stay within the context budget.%s The full history is preserved on the run log and can be replayed.]", len(elided), elisionDigest(elided))
	blocks = append(blocks, llm.Block{Kind: llm.KindText, Text: note})
	return llm.Message{Role: head.Role, Blocks: blocks}
}

// elisionDigest lists the distinct tool calls without a model call, so the view stays
// deterministic. It returns "" when no tools ran.
func elisionDigest(elided []llm.Message) string {
	const maxActions = 10
	var actions []string
	seen := map[string]bool{}
	for _, m := range elided {
		for _, b := range m.Blocks {
			if b.Kind != llm.KindToolUse || b.ToolUse == nil {
				continue
			}
			a := b.ToolUse.Name
			if tgt := toolArg(b.ToolUse.Input); tgt != "" {
				a += " " + tgt
			}
			if seen[a] {
				continue
			}
			seen[a] = true
			actions = append(actions, a)
		}
	}
	if len(actions) == 0 {
		return ""
	}
	more := ""
	if len(actions) > maxActions {
		more = fmt.Sprintf(", and %d more", len(actions)-maxActions)
		actions = actions[:maxActions]
	}
	return " Work done there: " + strings.Join(actions, "; ") + more + "."
}

// toolArg picks a path, pattern or command so a digest entry reads "read go.mod".
func toolArg(input json.RawMessage) string {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &a)
	switch {
	case a.Path != "":
		return a.Path
	case a.Pattern != "":
		return a.Pattern
	case a.Command != "":
		if len(a.Command) > 40 {
			return a.Command[:40]
		}
		return a.Command
	}
	return ""
}

func estimateTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += messageTokens(m)
	}
	return n
}

// messageTokens adds four tokens per block for structure.
func messageTokens(m llm.Message) int {
	chars := 0
	for _, b := range m.Blocks {
		chars += blockSize(b)
	}
	return chars/charsPerToken + len(m.Blocks)*4
}

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
	case llm.KindImage:
		return imageTokens * charsPerToken
	}
	return 0
}
