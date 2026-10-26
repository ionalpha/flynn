package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/fsatomic"
)

// ErrReadinessFileWritable reports a readiness file path that a confined command could
// write. A host that trusts the file to decide whether the runner is ready would trust a
// command's forgery of it.
var ErrReadinessFileWritable = errors.New("sandbox: readiness file is in a path a confined command can write")

// scratchRoots are the locations the confined view leaves writable for ordinary tooling
// besides the working directory (see confineMounts).
var scratchRoots = []string{"/tmp", "/dev/shm", "/var/tmp"}

// WriteReadinessFile records r as JSON at path, atomically and readable only by this
// user, for a supervisor that gates the runner's readiness on it. It writes the result of
// every check, refusals included, so a stale pass never outlives the host that earned it.
//
// The write fails with [ErrReadinessFileWritable] when path lies under the working
// directory or a writable directory a confined command is given ([Local.WritableRoots]),
// or under the scratch areas the confined view leaves writable, since a command could then
// write a pass. Both sides are compared with symbolic links resolved.
func WriteReadinessFile(path string, r Readiness, writable ...string) error {
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("sandbox: readiness file directory: %w", err)
	}
	target := filepath.Join(dir, filepath.Base(path))
	for _, root := range append(append([]string{}, writable...), scratchRoots...) {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		if pathWithin(target, root) {
			return fmt.Errorf("%w: %s is under %s", ErrReadinessFileWritable, target, root)
		}
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("sandbox: encode readiness: %w", err)
	}
	return fsatomic.WriteFile(target, append(b, '\n'), 0o600)
}

// WritableRoots lists every host directory a command confined by this Local can write:
// its working directory and the directories granted with [WithWritableDir]. A caller
// passes it to [WriteReadinessFile] so the readiness file lands outside them.
func (l *Local) WritableRoots() []string {
	return append([]string{l.root}, l.WritableDirs()...)
}

// pathWithin reports whether child is parent or a path nested under it, with both
// already absolute and cleaned.
func pathWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
