package main

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/internal/tui/fuzzy"
)

// fileCap bounds the completion universe. The walk stops here, so a giant
// tree costs a bounded scan instead of an unbounded one; fuzzy filtering
// still works over whatever was gathered.
const fileCap = 10000

// menuLimit is how many candidates one query returns: a menu's worth.
const menuLimit = 6

// pickBonus is the score added per prior acceptance of a candidate, sized
// above the matcher's structural bonuses so a repeatedly picked file rises
// to the top of equally good matches.
const pickBonus = 16

// fileCompleter serves @-completion over the files under one root. The
// universe is walked lazily and re-walked each time a completion session
// opens (the empty query, sent the moment @ is typed), so files the agent
// just created appear without a watcher. Accepted picks earn a ranking
// bonus for the rest of the session.
type fileCompleter struct {
	root string

	mu    sync.Mutex
	files []string
	picks map[string]int
}

func newFileCompleter(root string) *fileCompleter {
	return &fileCompleter{root: root, picks: make(map[string]int)}
}

// Complete implements app.Completer.
func (f *fileCompleter) Complete(query string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if query == "" || f.files == nil {
		f.files = listFiles(f.root, fileCap)
	}
	return fuzzy.Rank(query, f.files, menuLimit, func(s string) int {
		return pickBonus * f.picks[s]
	})
}

// Accepted implements app.Completer.
func (f *fileCompleter) Accepted(item string) {
	f.mu.Lock()
	f.picks[item]++
	f.mu.Unlock()
}

// listFiles walks root and returns up to limit file paths, relative to root
// with forward slashes, in the walk's deterministic lexical order. Dot
// directories (.git and friends) and dependency trees are skipped whole;
// dot files (.gitignore) stay. Unreadable entries are skipped, never fatal.
func listFiles(root string, limit int) []string {
	out := make([]string, 0, 256)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != root {
				return filepath.SkipDir
			}
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		name := d.Name()
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil //nolint:nilerr // an unrelatable path is skipped, not fatal
		}
		out = append(out, filepath.ToSlash(rel))
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	return out
}
