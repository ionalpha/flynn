package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

func (t readTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	abs, err := t.s.resolve(in.Path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the working root by resolve
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

func (t writeTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	abs, err := t.s.resolve(in.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(in.Content), 0o644); err != nil { //nolint:gosec // abs is confined to the working root; 0644 is intended for agent-written files
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), t.s.rel(abs)), nil
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

func (t editTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Old == "" {
		return "", fmt.Errorf("edit: 'old' must not be empty")
	}
	abs, err := t.s.resolve(in.Path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the working root by resolve
	if err != nil {
		return "", err
	}
	switch n := strings.Count(string(b), in.Old); {
	case n == 0:
		return "", fmt.Errorf("edit: 'old' not found in %s", t.s.rel(abs))
	case n > 1:
		return "", fmt.Errorf("edit: 'old' occurs %d times in %s; make it unique", n, t.s.rel(abs))
	}
	updated := strings.Replace(string(b), in.Old, in.New, 1)
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil { //nolint:gosec // abs is confined to the working root; 0644 is intended for agent-written files
		return "", err
	}
	return fmt.Sprintf("edited %s", t.s.rel(abs)), nil
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

func (t globTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(t.s.root, in.Pattern))
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if within(t.s.root, m) {
			out = append(out, t.s.rel(m))
		}
	}
	if len(out) == 0 {
		return "no matches", nil
	}
	sort.Strings(out)
	return strings.Join(out, "\n"), nil
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

func (t grepTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
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
	root := t.s.root
	if in.Path != "" {
		if root, err = t.s.resolve(in.Path); err != nil {
			return "", err
		}
	}

	var matches []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		b, err := os.ReadFile(p) //nolint:gosec // p is produced by WalkDir under the confined root
		if err != nil || isBinary(b) {
			return nil //nolint:nilerr // skip unreadable/binary files
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", t.s.rel(p), i+1, line))
				if len(matches) >= maxGrepMatches {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
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
