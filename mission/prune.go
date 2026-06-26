package mission

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// pruneResultThreshold is the size in bytes above which an older tool result is
// replaced by a one-line summary. A result at or below it is small enough that
// keeping it verbatim costs little, so only large outputs are touched.
const pruneResultThreshold = 600

// ResultSummarizer is an optional capability a Tool implements to describe a large
// result in one line. It is used when an older result is elided from the model's
// context to save tokens, so the model still sees what the call did without
// carrying its full output. A tool that does not implement it falls back to a
// generic size summary. The summary must be a pure function of the call so pruning
// stays deterministic and replayable, with no model round-trip.
type ResultSummarizer interface {
	SummarizeResult(input json.RawMessage, result string) string
}

// blockPos locates one content block within a message list.
type blockPos struct{ mi, bi int }

// toolCall is the call a tool result answers, recovered so a summary can name the
// tool and read its original arguments.
type toolCall struct {
	name  string
	input json.RawMessage
}

// pruneTranscript returns a token-lean view of a conversation to send to the model.
// Older and duplicate large tool results are replaced by one-line summaries, while
// the most recent result of each tool, every small result, and every error are kept
// verbatim. The point is that big tool outputs are the fastest way to exhaust the
// context budget, and an old one is rarely needed in full once newer work has moved
// on, yet the model still benefits from a one-line trace of what happened.
//
// It does not mutate msgs: the durable checkpoint stays the lossless source of
// truth, and this is a transient view over it. It never changes the message or
// block count, only the text of elided tool-result blocks, so tool-call/result
// pairing and the cacheable prefix layout are preserved. It is deterministic: the
// same transcript always prunes to the same view, so a cached prefix stays stable
// and a replay reproduces the view exactly. summarizer resolves a tool name to its
// optional one-line summarizer (nil when the tool has none).
func pruneTranscript(msgs []llm.Message, summarizer func(tool string) ResultSummarizer) []llm.Message {
	// Map each result back to the call that produced it.
	calls := map[string]toolCall{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolUse && b.ToolUse != nil {
				calls[b.ToolUse.ID] = toolCall{b.ToolUse.Name, b.ToolUse.Input}
			}
		}
	}

	// Find the most recent result per tool (kept verbatim) and the most recent
	// position of each distinct result body (so an earlier identical one is pruned as
	// a duplicate rather than re-summarized).
	latestByTool := map[string]blockPos{}
	latestByBody := map[uint64]blockPos{}
	for mi, m := range msgs {
		for bi, b := range m.Blocks {
			if b.Kind != llm.KindToolResult || b.ToolResult == nil {
				continue
			}
			latestByTool[calls[b.ToolResult.ToolUseID].name] = blockPos{mi, bi}
			latestByBody[hashString(b.ToolResult.Content)] = blockPos{mi, bi}
		}
	}

	out := make([]llm.Message, len(msgs))
	for mi, m := range msgs {
		out[mi] = m
		var blocks []llm.Block // allocated lazily, only if this message changes
		for bi, b := range m.Blocks {
			summary, pruned := pruneResult(b, blockPos{mi, bi}, calls, latestByTool, latestByBody, summarizer)
			if !pruned {
				continue
			}
			if blocks == nil {
				blocks = append([]llm.Block(nil), m.Blocks...) // copy before the first edit
			}
			r := *b.ToolResult // copy the result so the checkpoint's block is untouched
			r.Content = summary
			blocks[bi] = llm.Block{Kind: llm.KindToolResult, ToolResult: &r}
		}
		if blocks != nil {
			out[mi] = llm.Message{Role: m.Role, Blocks: blocks}
		}
	}
	return out
}

// pruneResult decides whether one block is an older large tool result that should be
// elided, and if so returns the replacement summary. A result is kept verbatim when
// it is not a tool result, is an error, is the most recent result of its tool, or is
// small. Otherwise it is replaced: by a duplicate note when an identical body
// appears later, else by the tool's one-line summary (or a generic size summary).
func pruneResult(
	b llm.Block, here blockPos,
	calls map[string]toolCall,
	latestByTool map[string]blockPos,
	latestByBody map[uint64]blockPos,
	summarizer func(tool string) ResultSummarizer,
) (string, bool) {
	if b.Kind != llm.KindToolResult || b.ToolResult == nil {
		return "", false
	}
	r := b.ToolResult
	if r.IsError || len(r.Content) <= pruneResultThreshold {
		return "", false // errors and small results are always kept in full
	}
	c := calls[r.ToolUseID]
	if latestByTool[c.name] == here {
		return "", false // the freshest result of each tool is kept in full
	}
	if dup := latestByBody[hashString(r.Content)]; dup != here {
		return fmt.Sprintf("[pruned %s: identical to a later result]", toolLabel(c.name)), true
	}
	body := ""
	if s := summarizer(c.name); s != nil {
		body = strings.TrimSpace(s.SummarizeResult(c.input, r.Content))
	}
	if body == "" {
		body = genericSummary(r.Content)
	}
	return fmt.Sprintf("[pruned %s: %s]", toolLabel(c.name), body), true
}

// genericSummary describes a result by its shape when a tool offers no summarizer,
// so something is always carried forward rather than an opaque elision.
func genericSummary(content string) string {
	return fmt.Sprintf("%d lines, %d chars", strings.Count(content, "\n")+1, len(content))
}

// toolLabel names the tool in a summary, falling back to a neutral word when a
// result could not be tied back to a call.
func toolLabel(name string) string {
	if name == "" {
		return "tool"
	}
	return name
}

// hashString is a fast, stable content fingerprint for duplicate detection. It is
// deterministic (no seed), so pruning a transcript is reproducible.
func hashString(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
