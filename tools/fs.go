package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

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
