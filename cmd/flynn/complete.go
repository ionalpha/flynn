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

// Frecency weights, blending how often and how recently a candidate was
// accepted. Both are sized above the matcher's structural bonuses so a picked
// file rises to the top of equally good matches. freqBonus rewards each prior
// acceptance; recencyBonus rewards the most recent one and decays by how many
// acceptances have happened since, so a file picked once just now can outrank
// one picked more times but longer ago. The exact values matter less than that
// a fresh single pick (recencyBonus) beats a stale handful (freqBonus per pick,
// no recency), which is what makes the ranking frecency rather than frequency.
const (
	freqBonus    = 8
	recencyBonus = 48
)

// pick records how a candidate has been accepted: the total count (frequency)
// and the acceptance sequence number of the most recent one (recency).
type pick struct {
	count   int
	lastSeq int
}

// fileCompleter serves @-completion over the files under one root. The
// universe is walked lazily and re-walked each time a completion session
// opens (the empty query, sent the moment @ is typed), so files the agent
// just created appear without a watcher. Accepted picks earn a frecency
// ranking bonus for the rest of the session.
type fileCompleter struct {
	root string

	mu    sync.Mutex
	files []string
	picks map[string]pick
	// seq counts acceptances so far, a monotonic logical clock. It is the
	// recency reference: a pick's lastSeq is compared against it, never against
	// wall-clock time, so the ranking stays deterministic and replayable.
	seq int
}

func newFileCompleter(root string) *fileCompleter {
	return &fileCompleter{root: root, picks: make(map[string]pick)}
}

// Complete implements app.Completer.
func (f *fileCompleter) Complete(query string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if query == "" || f.files == nil {
		f.files = listFiles(f.root, fileCap)
	}
	return fuzzy.Rank(query, f.files, menuLimit, f.frecency)
}

// frecency is the ranking bonus for a candidate: its acceptance frequency plus
// a recency term that decays as later acceptances push it into the past. A
// never-picked candidate scores zero, leaving pure fuzzy order.
func (f *fileCompleter) frecency(s string) int {
	p, ok := f.picks[s]
	if !ok {
		return 0
	}
	age := f.seq - p.lastSeq // 0 for the most recent pick, growing with each later one
	return freqBonus*p.count + recencyBonus/(1+age)
}

// Accepted implements app.Completer.
func (f *fileCompleter) Accepted(item string) {
	f.mu.Lock()
	f.seq++
	p := f.picks[item]
	p.count++
	p.lastSeq = f.seq
	f.picks[item] = p
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
