package watch

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// defaultMaxFileSize caps how large a file the walk reads when scanning for markers.
// A marker lives in a hand-edited source file; anything past this is a build
// artifact or data blob not worth reading on every tick.
const defaultMaxFileSize = 1 << 20 // 1 MiB

// Handler runs one detected marker. Returning nil means the marker was picked up and
// the watcher clears it from its file; a non-nil error leaves the file untouched and
// the marker is not retried until it is removed and re-added.
type Handler func(Marker) error

// Watcher polls a working tree for ai! / ai? markers and feeds each new one to a
// Handler exactly once, clearing it from its file after a successful pickup. It reads
// time through a clock.Timing so a Manual clock drives it deterministically in tests,
// mirroring the terminal-resize watcher. Markers are processed one at a time in the
// polling goroutine, so a marker never starts a run while an earlier one is still
// running.
type Watcher struct {
	root     string
	timing   clock.Timing
	interval time.Duration
	handle   Handler
	ignore   *Ignore
	maxSize  int64

	// seen keys a marker by file, line, and text so an unchanged marker fires once.
	// A key drops out of seen when its marker is no longer in the tree (cleared or
	// hand-removed), so re-adding the same request fires it again.
	seen map[string]struct{}

	stop chan struct{}
	done chan struct{}
}

// Start begins watching root, polling every interval and invoking handle for each new
// marker. It loads a root-level .gitignore if present and always skips .git. The
// returned Watcher runs until Stop is called.
func Start(timing clock.Timing, root string, interval time.Duration, handle Handler) *Watcher {
	w := &Watcher{
		root:     root,
		timing:   timing,
		interval: interval,
		handle:   handle,
		ignore:   loadIgnore(root),
		maxSize:  defaultMaxFileSize,
		seen:     map[string]struct{}{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go w.run()
	return w
}

// Stop ends the watch and waits for the polling goroutine to exit, so no handler can
// fire after Stop returns.
func (w *Watcher) Stop() {
	close(w.stop)
	<-w.done
}

func (w *Watcher) run() {
	defer close(w.done)
	for {
		timer := w.timing.NewTimer(w.interval)
		select {
		case <-w.stop:
			timer.Stop()
			return
		case <-timer.C():
		}
		w.tick()
	}
}

// tick scans the tree once, fires the handler for every marker not seen before, and
// forgets markers that have left the tree so a re-add fires again. It is exported to
// the package's tests as the unit of one poll; the run loop just paces it.
func (w *Watcher) tick() {
	current := w.scan()
	// Fire new markers in a stable order so multiple additions in one tick run
	// deterministically (by file, then line).
	keys := make([]string, 0, len(current))
	for k := range current {
		if _, ok := w.seen[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := current[keys[i]], current[keys[j]]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	for _, k := range keys {
		m := current[k]
		w.seen[k] = struct{}{}
		if err := w.handle(m); err == nil {
			// Cleared markers leave the tree, so forget the key at once: re-adding the
			// same request must fire it again. A handler that errored keeps its key so
			// the failed marker is not retried until it is removed and re-added.
			w.clearMarker(m)
			delete(w.seen, k)
		}
	}
	// Drop keys whose marker left the tree (hand-removed or edited into a new key), so
	// a still-remembered request can fire again once it reappears.
	for k := range w.seen {
		if _, ok := current[k]; !ok {
			delete(w.seen, k)
		}
	}
}

// scan walks the tree and returns every current marker keyed by file:line:text.
func (w *Watcher) scan() map[string]Marker {
	out := map[string]Marker{}
	_ = filepath.WalkDir(w.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A transiently unreadable entry skips itself rather than aborting the whole
			// scan; the next tick tries again.
			return nil //nolint:nilerr // skip this entry, keep walking the rest
		}
		// p is always under w.root (WalkDir invariant), so a plain prefix trim yields the
		// repo-relative path without filepath.Rel's cross-volume error path.
		rel := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(p, w.root), string(filepath.Separator)))
		if rel == "" {
			rel = "."
		}
		if d.IsDir() {
			if d.Name() == ".git" || (rel != "." && w.ignore.Match(rel, true)) {
				return fs.SkipDir
			}
			return nil
		}
		if w.ignore.Match(rel, false) {
			return nil
		}
		content, ok := w.readText(p)
		if !ok {
			return nil
		}
		for _, m := range Scan(rel, content) {
			out[markerKey(m)] = m
		}
		return nil
	})
	return out
}

// readText reads a file for scanning, skipping ones too large or that look binary (a
// NUL byte in the head), so the walk never pulls a build artifact into memory or
// tries to read a marker out of one.
func (w *Watcher) readText(p string) ([]byte, bool) {
	info, err := os.Stat(p)
	if err != nil || info.Size() > w.maxSize {
		return nil, false
	}
	content, err := os.ReadFile(p) //nolint:gosec // G304: watch scans files in the user's own working tree by design
	if err != nil {
		return nil, false
	}
	head := content
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, false
	}
	return content, true
}

// markerKey identifies a marker across ticks by where it is and what it asks, so an
// unchanged marker is fired once and an edited one is treated as new.
func markerKey(m Marker) string {
	return m.File + "\x00" + strconv.Itoa(m.Line) + "\x00" + string(m.Kind) + "\x00" + m.Text
}

// clearMarker removes a picked-up marker from its file. It re-reads the file so an
// edit since detection is respected, clears only the marker's line, and preserves the
// file mode. The marker's File is root-relative (the walk records it that way for
// provenance), so it is resolved against the watched root for IO. A failure is
// silent: the worst case is the marker fires again, never a corrupted file.
func (w *Watcher) clearMarker(m Marker) {
	abs := filepath.Join(w.root, filepath.FromSlash(m.File))
	content, err := os.ReadFile(abs) //nolint:gosec // G304: clears a marker in the user's own working tree by design
	if err != nil {
		return
	}
	next, changed := Clear(content, m.Line)
	if !changed {
		return
	}
	info, err := os.Stat(abs)
	mode := fs.FileMode(0o644)
	if err == nil {
		mode = info.Mode()
	}
	_ = os.WriteFile(abs, next, mode) //nolint:gosec // G703: abs is confined under the watched root; rewrites the file in place
}

// loadIgnore reads a root-level .gitignore, returning an empty matcher when there is
// none. Nested .gitignore files are not read; the always-skip of .git and the
// root-level rules cover the working trees watch mode runs over.
func loadIgnore(root string) *Ignore {
	content, err := os.ReadFile(filepath.Join(root, ".gitignore")) //nolint:gosec // G304: fixed .gitignore filename under the watched root
	if err != nil {
		return ParseIgnore(nil)
	}
	return ParseIgnore(content)
}
