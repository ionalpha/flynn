package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// maxGrepMatches caps grep output so a broad pattern cannot flood the model's
// context; the result notes when it was truncated.
const maxGrepMatches = 500

// --- read -------------------------------------------------------------------

type readTool struct{ s *Set }

func (readTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "read",
		Description: "Read a file's contents. Optionally start at a 1-based line offset and limit the number of lines returned.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["path"],
  "properties": {
    "path": {"type": "string", "description": "File path, relative to the working directory."},
    "offset": {"type": "integer", "description": "1-based line to start from. Omit to start at the beginning."},
    "limit": {"type": "integer", "description": "Maximum number of lines to return. Omit for the whole file."}
  },
  "additionalProperties": false
}`),
	}
}

func (t readTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	b, err := t.s.sb.ReadFile(ctx, in.Path)
	if err != nil {
		return "", err
	}
	if in.Offset <= 0 && in.Limit <= 0 {
		return string(b), nil
	}
	lines := strings.Split(string(b), "\n")
	start := 0
	if in.Offset > 0 {
		start = in.Offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if in.Limit > 0 && start+in.Limit < end {
		end = start + in.Limit
	}
	return strings.Join(lines[start:end], "\n"), nil
}

// --- write ------------------------------------------------------------------

type writeTool struct{ s *Set }

func (writeTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "write",
		Description: "Create or overwrite a file with the given contents. Parent directories are created as needed.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["path", "content"],
  "properties": {
    "path": {"type": "string", "description": "File path, relative to the working directory."},
    "content": {"type": "string", "description": "The full file contents to write."}
  },
  "additionalProperties": false
}`),
	}
}

func (t writeTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if err := t.s.sb.WriteFile(ctx, in.Path, []byte(in.Content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
}

// --- edit -------------------------------------------------------------------

type editTool struct{ s *Set }

func (editTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "edit",
		Description: "Replace an exact string in a file. The old string must appear exactly once, so the edit is unambiguous; the call fails otherwise.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["path", "old", "new"],
  "properties": {
    "path": {"type": "string", "description": "File path, relative to the working directory."},
    "old": {"type": "string", "description": "The exact text to replace. Must occur exactly once."},
    "new": {"type": "string", "description": "The text to replace it with."}
  },
  "additionalProperties": false
}`),
	}
}

func (t editTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Old == "" {
		return "", errors.New("edit: 'old' must not be empty")
	}
	b, err := t.s.sb.ReadFile(ctx, in.Path)
	if err != nil {
		return "", err
	}
	switch n := strings.Count(string(b), in.Old); {
	case n == 0:
		return "", fmt.Errorf("edit: 'old' not found in %s", in.Path)
	case n > 1:
		return "", fmt.Errorf("edit: 'old' occurs %d times in %s; make it unique", n, in.Path)
	}
	updated := strings.Replace(string(b), in.Old, in.New, 1)
	if err := t.s.sb.WriteFile(ctx, in.Path, []byte(updated)); err != nil {
		return "", err
	}
	return "edited " + in.Path, nil
}

// --- glob -------------------------------------------------------------------

type globTool struct{ s *Set }

func (globTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "glob",
		Description: "List files matching a glob pattern (e.g. 'src/*.go'), relative to the working directory.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["pattern"],
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern, relative to the working directory."}
  },
  "additionalProperties": false
}`),
	}
}

func (t globTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	matches, err := t.s.sb.Glob(ctx, in.Pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "no matches", nil
	}
	sort.Strings(matches)
	return strings.Join(matches, "\n"), nil
}

// --- grep -------------------------------------------------------------------

type grepTool struct{ s *Set }

func (grepTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "grep",
		Description: "Search file contents for a regular expression, recursively under an optional sub-path. Returns matching lines as path:line:text.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["pattern"],
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression to search for."},
    "path": {"type": "string", "description": "Sub-directory to search under, relative to the working directory. Omit to search everything."}
  },
  "additionalProperties": false
}`),
	}
}

func (t grepTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}
	root := in.Path
	if root == "" {
		root = "."
	}
	files, err := t.s.sb.Walk(ctx, root)
	if err != nil {
		return "", err
	}

	matches := make([]string, 0)
	truncated := false
	for _, f := range files {
		b, err := t.s.sb.ReadFile(ctx, f)
		if err != nil || isBinary(b) {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", f, i+1, line))
				if len(matches) >= maxGrepMatches {
					truncated = true
					break
				}
			}
		}
		if truncated {
			break
		}
	}
	if len(matches) == 0 {
		return "no matches", nil
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n... (truncated at %d matches)", maxGrepMatches)
	}
	return out, nil
}

// isBinary reports whether b looks like a binary file (a NUL byte in the first
// chunk), so grep skips it rather than emitting garbage.
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}
