package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"

	"github.com/ionalpha/flynn/internal/pathglob"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
)

const (
	defaultGlobResults = 500
	maxGlobResults     = 5000
)

type globTool struct{ s *Set }

func (globTool) Def() llm.Tool {
	return llm.Tool{
		Name: "glob",
		Description: "List paths matching a glob pattern, relative to the working directory. " +
			"* and ? stay within one directory, ** matches any depth, {a,b} and [...] work. " +
			"Version control directories and paths ignored by .gitignore are left out unless the pattern names them (e.g. 'dist/*.js').",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["pattern"],
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern, e.g. 'src/**/*.{ts,tsx}'."},
    "max_results": {"type": "integer", "description": "Most paths to return (default 500, at most 5000)."}
  },
  "additionalProperties": false
}`),
	}
}

func (t globTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Pattern    string `json:"pattern"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	pat, err := pathglob.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}

	bases, named, err := globRoots(pat)
	if err != nil {
		return "", fmt.Errorf("glob: %q: %w", in.Pattern, err)
	}

	found := map[string]bool{}
	partial, err := t.s.bounded(ctx, func(ctx context.Context) error {
		if err := statNamed(ctx, t.s.sb, named, found); err != nil {
			return err
		}
		for _, base := range bases {
			if err := globUnder(ctx, t.s.sb, base, pat, found); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}

	hits := slices.Sorted(maps.Keys(found))
	var left string
	if limit := clamp(in.MaxResults, defaultGlobResults, maxGlobResults); len(hits) > limit {
		left = fmt.Sprintf("%s%d more matches not shown; raise max_results or narrow the pattern]", notePrefix, len(hits)-limit)
		hits = hits[:limit]
	}
	return result(hits, []string{left, partial}), nil
}

// globRoots splits a pattern's alternatives into the directories to walk and the
// paths spelled out in full. Each alternative is searched from its leading
// literal directories, which the caller has named and so are not subject to
// ignore rules.
func globRoots(pat *pathglob.Pattern) (bases, named []string, err error) {
	walk, full := map[string]bool{}, map[string]bool{}
	for _, segs := range pat.Alternatives() {
		if slices.Contains(segs, "..") {
			return nil, nil, sandbox.ErrDenied
		}
		n := 0
		for n < len(segs) && pathglob.Literal(segs[n]) {
			n++
		}
		switch {
		case len(segs) == 0:
		case n == len(segs):
			full[path.Join(segs...)] = true
		case n == 0:
			walk["."] = true
		default:
			walk[path.Join(segs[:n]...)] = true
		}
	}
	return slices.Sorted(maps.Keys(walk)), slices.Sorted(maps.Keys(full)), nil
}

// statNamed adds the spelled-out paths that exist.
func statNamed(ctx context.Context, sb sandbox.Sandbox, named []string, found map[string]bool) error {
	for _, p := range named {
		n, err := treeFor(sb).stat(ctx, p)
		switch {
		case errors.Is(err, sandbox.ErrDenied):
			return err
		case err == nil && !n.implied:
			found[p] = true
		}
	}
	return nil
}

// globUnder adds the paths below base that match. Nothing can match under a base
// that is missing or is not a directory.
func globUnder(ctx context.Context, sb sandbox.Sandbox, base string, pat *pathglob.Pattern, found map[string]bool) error {
	tr := treeFor(sb)
	root, err := tr.stat(ctx, base)
	switch {
	case errors.Is(err, sandbox.ErrDenied):
		return err
	case err != nil || !root.dir:
		return nil //nolint:nilerr // a base that cannot be read matches nothing
	}
	w := walker{sb: sb, tree: tr, visit: func(n node) (bool, error) {
		if !n.implied && pat.Match(n.rel) {
			found[n.rel] = true
		}
		return pat.MayContain(n.rel), nil
	}}
	if err := w.walk(ctx, base); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
