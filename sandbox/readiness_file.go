package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/fsatomic"
)

// ErrReadinessFileWritable reports a path a confined command could write, and so
// forge a pass in.
var ErrReadinessFileWritable = errors.New("sandbox: readiness file is in a path a confined command can write")

// scratchRoots are what confineMounts leaves writable besides the working directory.
var scratchRoots = []string{"/tmp", "/dev/shm", "/var/tmp"}

// WriteReadinessFile writes r as JSON, atomically and owner-only. Refusals are
// written too, so a stale pass never survives. A path under [Local.WritableRoots]
// or the scratch areas fails with [ErrReadinessFileWritable]; symlinks are
// resolved on both sides.
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

// WritableRoots is the working directory plus every [WithWritableDir], for
// [WriteReadinessFile].
func (l *Local) WritableRoots() []string {
	return append([]string{l.root}, l.WritableDirs()...)
}

// pathWithin takes absolute, cleaned paths; child may equal parent.
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
