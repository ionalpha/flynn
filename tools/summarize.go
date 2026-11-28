package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/internal/text"

	"github.com/ionalpha/flynn/mission"
)

// A summary replaces a result elided from the model's context. It is computed
// from the call alone, so eliding is deterministic. Short-output tools (write,
// edit) get a generic note. The assertions fail the build if a signature
// drifts.
var (
	_ mission.ResultSummarizer = bashTool{}
	_ mission.ResultSummarizer = readTool{}
	_ mission.ResultSummarizer = globTool{}
	_ mission.ResultSummarizer = grepTool{}
)

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
	return fmt.Sprintf("%q, %s, %d lines", text.Clip(firstLine(in.Command), 48), status, lineCount(result))
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
	return fmt.Sprintf("%q, %d files", in.Pattern, hitCount(result))
}

// SummarizeResult describes a search by its pattern and how many lines matched.
func (grepTool) SummarizeResult(input json.RawMessage, result string) string {
	var in struct {
		Pattern string `json:"pattern"`
	}
	_ = json.Unmarshal(input, &in)
	return fmt.Sprintf("%q, %d matches", in.Pattern, hitCount(result))
}

// hitCount counts a search result's lines, leaving out its notes and the
// separators between context groups.
func hitCount(result string) int {
	n := lineCount(result)
	for line := range strings.SplitSeq(result, "\n") {
		if line == "--" || line == "no matches" || strings.HasPrefix(line, notePrefix) {
			n--
		}
	}
	return n
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}
