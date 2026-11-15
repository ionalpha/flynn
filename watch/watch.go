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

// defaultMaxFileSize caps the files the walk reads. Markers live in hand-edited
// source; a larger file is a build artifact or data not worth reading every tick.
const defaultMaxFileSize = 1 << 20 // 1 MiB

// Handler runs one detected marker. On nil the watcher clears the marker from its
// file; on error the file is left alone and the marker is not retried until it is
// removed and re-added.
type Handler func(Marker) error

// Watcher polls a working tree and feeds each new marker to a [Handler] once.
// Markers run one at a time on the polling goroutine, so a marker never starts a
// run while an earlier one is still running.
type Watcher struct {
	root     string
	timing   clock.Timing
	interval time.Duration
	handle   Handler
	ignore   *Ignore
	maxSize  int64

	// seen holds the keys of markers already fired. A key leaves seen when its
	// marker leaves the tree, so re-adding the same request fires it again.
	seen map[string]struct{}

	stop chan struct{}
	done chan struct{}
}

// Start polls root every interval until [Watcher.Stop]. It honours a root-level
// .gitignore and always skips .git.
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

// Stop waits for the polling goroutine to exit, so no handler fires after it
// returns.
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

// tick is one poll; the package's tests call it directly.
func (w *Watcher) tick() {
	current := w.scan()
	// File then line order, so several markers added in one tick run deterministically.
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
			// A cleared marker forgets its key at once so a re-add fires again. A failed
			// one keeps its key so it is not retried in place.
			w.clearMarker(m)
			delete(w.seen, k)
		}
	}
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
			// An unreadable entry is skipped; the next tick tries again.
			return nil //nolint:nilerr // skip this entry, keep walking the rest
		}
		// WalkDir keeps p under w.root, so a prefix trim is enough and avoids
		// filepath.Rel's cross-volume error.
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

// readText skips files over maxSize and files with a NUL byte in the first 8000
// bytes, which are treated as binary.
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

// markerKey includes the text, so an edited marker counts as new.
func markerKey(m Marker) string {
	return m.File + "\x00" + strconv.Itoa(m.Line) + "\x00" + string(m.Kind) + "\x00" + m.Text
}

// clearMarker re-reads the file so an edit since detection is respected, and keeps
// the file mode. A failure is silent: the worst case is the marker fires again.
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

// loadIgnore reads only the root-level .gitignore; nested ones are not read.
func loadIgnore(root string) *Ignore {
	content, err := os.ReadFile(filepath.Join(root, ".gitignore")) //nolint:gosec // G304: fixed .gitignore filename under the watched root
	if err != nil {
		return ParseIgnore(nil)
	}
	return ParseIgnore(content)
}
