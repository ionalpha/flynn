package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/mission"
)

// The large-output tools provide a one-line summarizer; this is asserted at compile
// time so the pruning path keeps its rich summaries if a signature drifts.
var (
	_ mission.ResultSummarizer = bashTool{}
	_ mission.ResultSummarizer = readTool{}
	_ mission.ResultSummarizer = globTool{}
	_ mission.ResultSummarizer = grepTool{}
)

// This file gives the tools whose output can grow large a one-line summary of a
// call, used when an older result is elided from the model's context to keep the
// token budget in check. Each summary is a pure function of the call's arguments
// and result, so eliding is deterministic and adds no model round-trip. Tools whose
// results are always short (write, edit) need none and fall back to a generic note.

// SummarizeResult describes a shell run by its command and output size.
func (bashTool) SummarizeResult(input json.RawMessage, result string) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	status := "exit 0"
	if i := strings.LastIndex(result, "[exit status "); i >= 0 {
		status = strings.TrimSuffix(strings.TrimPrefix(result[i:], "["), "]")
	}
	return fmt.Sprintf("%q, %s, %d lines", clip(firstLine(in.Command), 48), status, lineCount(result))
}

// SummarizeResult describes a file read by its path and size.
func (readTool) SummarizeResult(input json.RawMessage, result string) string {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	return fmt.Sprintf("%s, %d lines", in.Path, lineCount(result))
}

// SummarizeResult describes a glob by its pattern and how many files matched.
func (globTool) SummarizeResult(input json.RawMessage, result string) string {
	var in struct {
		Pattern string `json:"pattern"`
	}
	_ = json.Unmarshal(input, &in)
	return fmt.Sprintf("%q, %d files", in.Pattern, lineCount(result))
}

// SummarizeResult describes a search by its pattern and how many lines matched.
func (grepTool) SummarizeResult(input json.RawMessage, result string) string {
	var in struct {
		Pattern string `json:"pattern"`
	}
	_ = json.Unmarshal(input, &in)
	return fmt.Sprintf("%q, %d matches", in.Pattern, lineCount(result))
}

// firstLine is the first line of s, so a multi-line command summarizes by its head.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// lineCount counts the lines in s (1 for a non-empty single line, 0 for empty), so a
// summary can state a result's size without echoing it.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// clip shortens s to at most n runes, marking a cut with an ellipsis, so a long
// command does not blow up a one-line summary.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
