package mission

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// pruneResultThreshold is in bytes; a result at or below it is cheap to keep verbatim.
const pruneResultThreshold = 600

// ResultSummarizer is an optional [Tool] method that describes an elided result in one
// line. It must be a pure function of the call, so pruning stays replayable.
type ResultSummarizer interface {
	SummarizeResult(input json.RawMessage, result string) string
}

type blockPos struct{ mi, bi int }

type toolCall struct {
	name  string
	input json.RawMessage
}

// key names a call by its tool and its exact input bytes. Two spellings of the same
// arguments count as two calls, which keeps a result that could have been pruned and
// never prunes one that is still the latest.
func (c toolCall) key() string { return c.name + "\x00" + string(c.input) }

// pruneTranscript summarizes superseded and duplicate large tool results in one line,
// keeping the latest result of each distinct call, small results and errors verbatim,
// because large outputs exhaust the budget fastest. A call is its tool and its input, so
// a rerun of a command supersedes the earlier run while a read of one file never hides a
// read of another: an agent needs the code and its test in view together to edit, and
// one sent back to re-read is stopped as stalled. Overflow is [compactView]'s job.
//
// It never mutates msgs, since the checkpoint is the lossless record, and never changes
// the message or block count, so call/result pairing and the cached prefix survive. It
// is deterministic, so a replay reproduces the view.
func pruneTranscript(msgs []llm.Message, summarizer func(tool string) ResultSummarizer) []llm.Message {
	// This runs every turn over the whole history, and most turns hold no large result,
	// so one scan without allocation skips the indexing below.
	if !hasPrunableResult(msgs) {
		return msgs
	}

	calls := indexCalls(msgs)
	idx := indexResults(msgs, calls)

	out := make([]llm.Message, len(msgs))
	for mi, m := range msgs {
		out[mi] = m
		var blocks []llm.Block // allocated lazily, only if this message changes
		for bi, b := range m.Blocks {
			summary, pruned := pruneResult(b, blockPos{mi, bi}, calls, idx, summarizer)
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

func indexCalls(msgs []llm.Message) map[string]toolCall {
	calls := map[string]toolCall{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == llm.KindToolUse && b.ToolUse != nil {
				calls[b.ToolUse.ID] = toolCall{b.ToolUse.Name, b.ToolUse.Input}
			}
		}
	}
	return calls
}

type resultIndex struct {
	latestByCall map[string]blockPos
	// latestByBody lets an earlier identical body be pruned as a duplicate.
	latestByBody map[uint64]blockPos
	// bodyHash is computed once while indexing.
	bodyHash map[blockPos]uint64
}

// indexResults hashes only large bodies, because only large results are pruned.
func indexResults(msgs []llm.Message, calls map[string]toolCall) resultIndex {
	idx := resultIndex{
		latestByCall: map[string]blockPos{},
		latestByBody: map[uint64]blockPos{},
		bodyHash:     map[blockPos]uint64{},
	}
	for mi, m := range msgs {
		for bi, b := range m.Blocks {
			if b.Kind != llm.KindToolResult || b.ToolResult == nil {
				continue
			}
			here := blockPos{mi, bi}
			idx.latestByCall[calls[b.ToolResult.ToolUseID].key()] = here
			if len(b.ToolResult.Content) > pruneResultThreshold {
				h := hashString(b.ToolResult.Content)
				idx.bodyHash[here] = h
				idx.latestByBody[h] = here
			}
		}
	}
	return idx
}

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

// pruneResult prefers a duplicate note when an identical body appears later, then the
// tool's summary, then a size summary.
func pruneResult(
	b llm.Block, here blockPos,
	calls map[string]toolCall,
	idx resultIndex,
	summarizer func(tool string) ResultSummarizer,
) (string, bool) {
	if b.Kind != llm.KindToolResult || b.ToolResult == nil {
		return "", false
	}
	r := b.ToolResult
	if r.IsError || len(r.Content) <= pruneResultThreshold {
		return "", false
	}
	c := calls[r.ToolUseID]
	if idx.latestByCall[c.key()] == here {
		return "", false
	}
	if dup := idx.latestByBody[idx.bodyHash[here]]; dup != here {
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

func genericSummary(content string) string {
	return fmt.Sprintf("%d lines, %d chars", strings.Count(content, "\n")+1, len(content))
}

// toolLabel covers a result that could not be tied back to a call.
func toolLabel(name string) string {
	if name == "" {
		return "tool"
	}
	return name
}

// hashString is FNV-1a, unseeded so pruning replays, and identical to hash/fnv without the
// []byte copy hash.Hash forces on every body of every turn.
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
