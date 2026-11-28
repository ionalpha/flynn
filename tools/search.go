package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ionalpha/flynn/internal/pathglob"
	"github.com/ionalpha/flynn/sandbox"
)

// defaultSearchTime bounds one grep or glob call; at the bound it returns what it
// has and says so.
const defaultSearchTime = time.Minute

// notePrefix starts every line of a search result that is about the result.
const notePrefix = "... ["

// vcsDirs hold version control metadata, which a search never enters.
var vcsDirs = []string{".git", ".hg", ".svn", ".jj"}

// ignoreFiles are read from every searched directory, later ones taking
// precedence.
var ignoreFiles = []string{".gitignore", ".ignore"}

// excludeFile is the repository's private ignore file, ranking below the others.
const excludeFile = ".git/info/exclude"

// node is one directory entry as a search sees it.
type node struct {
	rel  string // relative to the sandbox root, slash-separated
	dir  bool
	mode fs.FileMode // type bits; zero for a listing backend
	// size is -1 when the backend cannot say before reading.
	size int64
	// implied marks a directory inferred from the files under it.
	implied bool
}

// tree is the view of a sandbox a search walks.
type tree interface {
	stat(ctx context.Context, p string) (node, error)
	readDir(ctx context.Context, dir string) ([]node, error)
}

// treeFor reads directory by directory where the sandbox can, and otherwise
// lists everything under base once.
func treeFor(sb sandbox.Sandbox) tree {
	if r, ok := sb.(sandbox.DirReader); ok {
		return dirTree{r}
	}
	return &listTree{sb: sb}
}

type dirTree struct{ r sandbox.DirReader }

func (t dirTree) stat(ctx context.Context, p string) (node, error) {
	info, err := t.r.Stat(ctx, p)
	if err != nil {
		return node{}, err
	}
	return node{rel: p, dir: info.IsDir(), mode: info.Mode().Type(), size: info.Size()}, nil
}

func (t dirTree) readDir(ctx context.Context, dir string) ([]node, error) {
	entries, err := t.r.ReadDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]node, 0, len(entries))
	for _, e := range entries {
		n := node{rel: path.Join(dir, e.Name()), dir: e.IsDir(), mode: e.Type(), size: -1}
		if info, err := e.Info(); err == nil && e.Type().IsRegular() {
			n.size = info.Size()
		}
		out = append(out, n)
	}
	return out, nil
}

// listTree rebuilds directories from a flat listing. The first path it is asked
// about is the one it lists.
type listTree struct {
	sb   sandbox.Sandbox
	dirs map[string][]node
	file map[string]bool
}

func (t *listTree) load(ctx context.Context, base string) error {
	if t.dirs != nil {
		return nil
	}
	files, err := t.sb.Walk(ctx, base)
	if err != nil {
		return err
	}
	t.dirs, t.file = map[string][]node{base: nil}, map[string]bool{}
	listed := map[string]bool{}
	for _, f := range files {
		t.file[f] = true
		for child, isDir := f, false; child != base && child != "." && !listed[child]; child, isDir = path.Dir(child), true {
			listed[child] = true
			parent := path.Dir(child)
			t.dirs[parent] = append(t.dirs[parent], node{rel: child, dir: isDir, size: -1, implied: isDir})
		}
	}
	for _, entries := range t.dirs {
		slices.SortFunc(entries, func(a, b node) int { return strings.Compare(a.rel, b.rel) })
	}
	return nil
}

func (t *listTree) stat(ctx context.Context, p string) (node, error) {
	if err := t.load(ctx, p); err != nil {
		return node{}, err
	}
	switch {
	case t.file[p]:
		return node{rel: p, size: -1}, nil
	case len(t.dirs[p]) > 0 || p == ".":
		return node{rel: p, dir: true, size: -1, implied: true}, nil
	}
	return node{}, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
}

func (t *listTree) readDir(_ context.Context, dir string) ([]node, error) {
	return t.dirs[dir], nil
}

// cleanPath puts a caller's path in the root-relative slash form. A path that
// climbs out keeps its leading "..", so the sandbox still refuses it.
func cleanPath(p string) string {
	p = strings.TrimPrefix(path.Clean(filepath.ToSlash(p)), "/")
	if p == "" {
		return "."
	}
	return p
}

// ruleSet is the ignore file rules of one directory.
type ruleSet struct {
	dir string
	ig  *pathglob.Ignore
}

func (r ruleSet) relative(p string) string {
	if r.dir == "." {
		return p
	}
	return strings.TrimPrefix(p, r.dir+"/")
}

// ignored asks the deepest ignore file first, as git does.
func ignored(rules []ruleSet, n node) bool {
	for _, r := range slices.Backward(rules) {
		if skip, matched := r.ig.Decide(r.relative(n.rel), n.dir); matched {
			return skip
		}
	}
	return false
}

// walker visits what lies under a base directory, skipping version control
// directories and ignored paths. The base itself is not judged: a caller who
// names a path gets it, ignored or not.
type walker struct {
	sb   sandbox.Sandbox
	tree tree
	// visit is called for each entry. For a directory, false prunes it.
	visit func(n node) (descend bool, err error)
}

func (w walker) walk(ctx context.Context, base string) error {
	var rules []ruleSet
	if base != "." {
		dir := "."
		for part := range strings.SplitSeq(base, "/") {
			names := ignoreFiles
			if dir == "." {
				names = append([]string{excludeFile}, ignoreFiles...)
			}
			rules = w.addRules(ctx, rules, dir, names)
			dir = path.Join(dir, part)
		}
	}
	return w.dir(ctx, base, rules)
}

// addRules reads the named ignore files of dir; an unreadable one has no rules.
func (w walker) addRules(ctx context.Context, rules []ruleSet, dir string, names []string) []ruleSet {
	ig := pathglob.ParseIgnore(nil)
	for _, name := range names {
		if b, err := w.sb.ReadFile(ctx, path.Join(dir, name)); err == nil {
			ig.Add(b)
		}
	}
	if ig.Empty() {
		return rules
	}
	return append(rules, ruleSet{dir: dir, ig: ig})
}

func (w walker) dir(ctx context.Context, dir string, rules []ruleSet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := w.tree.readDir(ctx, dir)
	if err != nil {
		return err
	}
	has := func(name string, isDir bool) bool {
		return slices.ContainsFunc(entries, func(n node) bool {
			return n.dir == isDir && (isDir || n.mode.IsRegular()) && path.Base(n.rel) == name
		})
	}
	var names []string
	if dir == "." && has(".git", true) {
		names = append(names, excludeFile)
	}
	for _, name := range ignoreFiles {
		if has(name, false) {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		rules = w.addRules(ctx, rules, dir, names)
	}
	for _, n := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if slices.Contains(vcsDirs, path.Base(n.rel)) || ignored(rules, n) {
			continue
		}
		descend, err := w.visit(n)
		if err != nil {
			return err
		}
		if n.dir && descend {
			// An unreadable subdirectory is skipped; a finished context is not.
			if err := w.dir(ctx, n.rel, rules); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}

// bounded runs search under the call's time limit. partial reports that the
// limit, and not the caller, stopped it.
func (s *Set) bounded(ctx context.Context, search func(context.Context) error) (partial string, err error) {
	limit := s.searchTime
	if limit == 0 {
		limit = defaultSearchTime
	}
	sctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err = search(sctx)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(sctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("%ssearch stopped after %s; results are partial; narrow the path or pattern]", notePrefix, limit), nil
	}
	return "", err
}

// result joins the hits and the notes about them.
func result(hits, notes []string) string {
	if len(hits) == 0 {
		hits = []string{"no matches"}
	}
	for _, n := range notes {
		if n != "" {
			hits = append(hits, n)
		}
	}
	return strings.Join(hits, "\n")
}

// clamp returns v, or def when v is not positive, held to ceiling.
func clamp(v, def, ceiling int) int {
	if v <= 0 {
		v = def
	}
	return min(v, ceiling)
}
