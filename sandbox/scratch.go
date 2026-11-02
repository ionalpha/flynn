package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// scratchState is the sandbox's scratch directory: created on the first confined launch and
// removed on Close, so a Local that never runs a confined command leaves nothing on disk.
type scratchState struct {
	mu  sync.Mutex
	dir string
}

// scratchTempName is the scratch subdirectory the temp variables point at. The caches sit
// beside it, not inside it, so a tool that empties its temp directory leaves the caches
// alone.
const scratchTempName = "tmp"

// scratchCaches maps the cache variables of toolchains that default under the user's home
// directory, where a confined child cannot write, to a subdirectory of the scratch
// directory. GOCACHE is what lets `go test` run at all. GOMODCACHE is redirected because the
// host's module cache is read-only to the child, and downloads then stay out of a cache that
// would outlive the sandbox. CARGO_HOME is left alone: it holds registry configuration and
// installed binaries, so redirecting it changes what cargo does, not only where it writes.
var scratchCaches = map[string]string{
	"GOCACHE":          "go-build",
	"GOMODCACHE":       "go-mod",
	"npm_config_cache": "npm",
	"PIP_CACHE_DIR":    "pip",
}

// scratchTempKeys are the variables that name a process's temp directory.
func scratchTempKeys() []string {
	if runtime.GOOS == "windows" {
		return []string{"TEMP", "TMP"}
	}
	return []string{"TMPDIR"}
}

// scratchDir returns the sandbox's scratch directory, creating it on first use. It is the
// second writable location a confined child has besides the workspace: caches and temp
// files only, private to this sandbox, and deleted on Close. It lives outside the
// workspace so the workspace is never the temp root (Go ignores a go.mod found there) and
// so build output does not land in the user's project.
//
// It is a host directory granted to the child, not the private tmpfs Linux mounts at /tmp.
// A toolchain cache has to outlive one command, because `go mod download` followed by
// `go test` is two launches and each launch builds its own mount namespace, and a build
// cache or module tree can outgrow a memory-backed mount. The cost is that the directory
// is real disk, so Close removes it.
func (l *Local) scratchDir() (string, error) {
	l.scratch.mu.Lock()
	defer l.scratch.mu.Unlock()
	if l.scratch.dir != "" {
		return l.scratch.dir, nil
	}
	dir, err := makeScratchDir()
	if err != nil {
		return "", fmt.Errorf("create scratch directory: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if err := os.Mkdir(filepath.Join(dir, scratchTempName), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create scratch temp directory: %w", err)
	}
	l.scratch.dir = dir
	return dir, nil
}

// makeScratchDir creates the directory under the host's temp root, except on Linux, which
// prefers /var/tmp. The confined view mounts a private tmpfs over /tmp and /dev/shm, and a
// scratch directory under either would be hidden by that mount, or would stop the mount
// being made at all, leaving the child without a writable /tmp. /var/tmp is the standard
// temp location nothing is mounted over; a host without it falls back to the default.
func makeScratchDir() (string, error) {
	if runtime.GOOS == "linux" {
		if dir, err := os.MkdirTemp("/var/tmp", "flynn-scratch-"); err == nil {
			return dir, nil
		}
	}
	return os.MkdirTemp("", "flynn-scratch-")
}

// removeScratch deletes the scratch directory. Tools such as the Go module cache mark what
// they write read-only, which a removal cannot get past (Windows will not delete the file,
// and elsewhere the directory cannot be emptied), so a failed removal makes the tree
// writable and tries again.
func (l *Local) removeScratch() error {
	l.scratch.mu.Lock()
	defer l.scratch.mu.Unlock()
	dir := l.scratch.dir
	if dir == "" {
		return nil
	}
	l.scratch.dir = ""
	if err := os.RemoveAll(dir); err == nil { //nolint:gosec // G703: dir is the directory this sandbox created with MkdirTemp, never caller input
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error { //nolint:gosec // G703: dir is the sandbox's own MkdirTemp result
		if err == nil {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: clearing read-only so removal can proceed; the tree is deleted next
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil { //nolint:gosec // G703: dir is the directory this sandbox created with MkdirTemp, never caller input
		return fmt.Errorf("sandbox: remove scratch directory: %w", err)
	}
	return nil
}

// withScratchEnv points the temp variables and the toolchain cache variables at scratch.
// The host's own temp value (hostTemp, which the baseline copies in) is replaced; a value
// that differs from the host's was granted by the operator and stays. A cache variable is
// set only when absent, since the baseline never carries one and so any present was
// granted. Windows treats variable names case-insensitively, so presence is checked that
// way there.
func withScratchEnv(m map[string]string, scratch string, hostTemp func(string) string) {
	has := func(key string) bool {
		if runtime.GOOS != "windows" {
			_, ok := m[key]
			return ok
		}
		for k := range m {
			if strings.EqualFold(k, key) {
				return true
			}
		}
		return false
	}
	for _, k := range scratchTempKeys() {
		if v, ok := m[k]; !ok || v == hostTemp(k) {
			m[k] = filepath.Join(scratch, scratchTempName)
		}
	}
	for k, sub := range scratchCaches {
		if !has(k) {
			m[k] = filepath.Join(scratch, sub)
		}
	}
}

// writableTargets are the directories a confined launch grants the child write access to:
// the ones the caller named with [WithWritableDir] and the scratch directory. It creates
// the scratch directory, so every launch path that grants before it builds the environment
// finds it in place.
func (l *Local) writableTargets() ([]string, error) {
	scratch, err := l.scratchDir()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWriteGrant, err)
	}
	return append([]string{scratch}, l.writableDirs...), nil
}
