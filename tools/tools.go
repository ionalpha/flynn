// Package tools is the agent's default toolset: the standard capable surface a
// coding agent needs to do real work - run a command, and read, write, edit,
// glob, and grep files - each exposed as a mission.Tool the model can call.
//
// Every tool is rooted at a working directory and every file path the model
// supplies is confined to it: a path that escapes the root (via "..", an absolute
// path, or a symlink that points outside) is rejected before any I/O. That makes
// the toolset safe to hand untrusted model output even before the heavier sandbox
// backends (worktrees, containers) land, which attach at the run level above this.
package tools

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/mission"
)

// Set is the default toolset rooted at a working directory. Construct it with New
// and hand its tools to a mission executor.
type Set struct {
	root string // absolute, symlinks resolved
}

// New builds a toolset confined to root. The root is resolved to an absolute,
// symlink-free path once, so confinement checks compare against a stable base.
func New(root string) (*Set, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("tools: resolve root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Set{root: abs}, nil
}

// Root returns the absolute working directory the toolset is confined to.
func (s *Set) Root() string { return s.root }

// Tools returns the full default toolset as mission.Tools, ready to register with
// an executor.
func (s *Set) Tools() []mission.Tool {
	return []mission.Tool{
		bashTool{s},
		readTool{s},
		writeTool{s},
		editTool{s},
		globTool{s},
		grepTool{s},
	}
}

// ErrPathEscapes is returned when a model-supplied path resolves outside the root.
type ErrPathEscapes struct{ Path string }

func (e *ErrPathEscapes) Error() string {
	return fmt.Sprintf("path %q escapes the working directory", e.Path)
}

// resolve confines a model-supplied path to the root and returns the absolute path
// to operate on. Input is treated as relative to the root (an absolute input is
// re-anchored under it), the result is cleaned and checked to stay within root
// lexically, and the nearest existing ancestor is symlink-resolved and re-checked
// so a symlink cannot point the operation outside the root.
func (s *Set) resolve(p string) (string, error) {
	abs := filepath.Clean(filepath.Join(s.root, p))
	if !within(s.root, abs) {
		return "", &ErrPathEscapes{p}
	}
	// Walk up to the nearest path that exists and verify it (and therefore abs)
	// does not reach outside root through a symlink.
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !within(s.root, resolved) {
				return "", &ErrPathEscapes{p}
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break // reached the filesystem root without resolving; lexical check stands
		}
		probe = parent
	}
	return abs, nil
}

// within reports whether p is the root or lies inside it.
func within(root, p string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rel renders an absolute path back as root-relative for tool output, so the model
// sees the same paths it passes in rather than absolute host paths.
func (s *Set) rel(abs string) string {
	if r, err := filepath.Rel(s.root, abs); err == nil {
		return filepath.ToSlash(r)
	}
	return abs
}
