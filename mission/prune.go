package mission

import (
	"encoding/json"
	"fmt"
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
	// Fast path: only a large, non-error tool result can ever be elided, so if the
	// transcript holds none there is nothing to prune. The common turn carries only
	// small results, and this pass runs on every turn over the whole growing history,
	// so skipping the map-building and hashing below keeps such a turn a single cheap
	// scan with no allocation. The original slice is returned as a read-only view (as
	// it would be when nothing changes), which callers never mutate.
	if !hasPrunableResult(msgs) {
		return msgs
	}

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
	// position of each distinct large result body (so an earlier identical one is
	// pruned as a duplicate rather than re-summarized). Only large bodies are hashed:
	// a body can only match another of the same length, and pruning is decided for
	// large results alone, so small results never populate the duplicate index. Each
	// large body is hashed exactly once here and its hash reused in pruneResult.
	latestByTool := map[string]blockPos{}
	latestByBody := map[uint64]blockPos{}
	bodyHash := map[blockPos]uint64{}
	for mi, m := range msgs {
		for bi, b := range m.Blocks {
			if b.Kind != llm.KindToolResult || b.ToolResult == nil {
				continue
			}
			here := blockPos{mi, bi}
			latestByTool[calls[b.ToolResult.ToolUseID].name] = here
			if len(b.ToolResult.Content) > pruneResultThreshold {
				h := hashString(b.ToolResult.Content)
				bodyHash[here] = h
				latestByBody[h] = here
			}
		}
	}

	out := make([]llm.Message, len(msgs))
	for mi, m := range msgs {
		out[mi] = m
		var blocks []llm.Block // allocated lazily, only if this message changes
		for bi, b := range m.Blocks {
			summary, pruned := pruneResult(b, blockPos{mi, bi}, calls, latestByTool, latestByBody, bodyHash, summarizer)
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

// hasPrunableResult reports whether any block is a non-error tool result large enough
// to be elided. Only such a result is ever pruned, so when none is present the whole
// prune pass is a no-op and is skipped.
func hasPrunableResult(msgs []llm.Message) bool {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolResult && b.ToolResult != nil &&
				!b.ToolResult.IsError && len(b.ToolResult.Content) > pruneResultThreshold {
				return true
			}
		}
	}
	return false
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
	bodyHash map[blockPos]uint64,
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
	// here is a large result, so its body was hashed once when the duplicate index was
	// built; reuse that hash rather than hashing the body again.
	if dup := latestByBody[bodyHash[here]]; dup != here {
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
// FNV-1a computed directly over the string's bytes: deterministic (no seed) so
// pruning a transcript is reproducible on replay, and byte-identical to hash/fnv,
// but without the []byte(s) copy that hashing through the hash.Hash interface forces
// on every result body. This runs on the whole growing transcript each turn, so
// dropping that per-body allocation matters over a long tool-using loop.
func hashString(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
