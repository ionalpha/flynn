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

// scratchState is created on the first confined launch, so a Local that never
// confines leaves nothing on disk.
type scratchState struct {
	mu  sync.Mutex
	dir string
}

// scratchTempName holds temp files apart from the caches, so a tool that empties
// its temp directory leaves the caches alone.
const scratchTempName = "tmp"

// scratchCaches redirects caches that default under the home directory, which a
// confined child cannot write. Without GOCACHE `go test` cannot run. GOMODCACHE
// moves because the host module cache is read-only to the child. CARGO_HOME is
// absent because it holds registry config and installed binaries, so moving it
// changes what cargo does.
var scratchCaches = map[string]string{
	"GOCACHE":          "go-build",
	"GOMODCACHE":       "go-mod",
	"npm_config_cache": "npm",
	"PIP_CACHE_DIR":    "pip",
}

// scratchTempKeys are the variables that name a process's temp directory on goos.
func scratchTempKeys(goos string) []string {
	if goos == "windows" {
		return []string{"TEMP", "TMP"}
	}
	return []string{"TMPDIR"}
}

// scratchDir is the child's second writable location, deleted on Close. It sits
// outside the workspace because Go ignores a go.mod in the temp root. It is a host
// directory because a cache must outlive one launch (`go mod download` then
// `go test` are two mount namespaces) and can outgrow a tmpfs.
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

// makeScratchDir prefers /var/tmp on Linux, because the confined view mounts a
// tmpfs over /tmp and /dev/shm that would hide the directory or fail to mount.
func makeScratchDir() (string, error) {
	if runtime.GOOS == "linux" {
		if dir, err := mkdirTemp("/var/tmp", "flynn-scratch-"); err == nil {
			return dir, nil
		}
	}
	return mkdirTemp("", "flynn-scratch-")
}

// mkdirTemp and removeAll are variables so a test can make them fail.
var (
	mkdirTemp = os.MkdirTemp
	removeAll = os.RemoveAll
)

// removeScratch retries after making the tree writable, because the Go module
// cache writes read-only files that block removal.
func (l *Local) removeScratch() error {
	l.scratch.mu.Lock()
	defer l.scratch.mu.Unlock()
	dir := l.scratch.dir
	if dir == "" {
		return nil
	}
	l.scratch.dir = ""
	if err := removeAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error { //nolint:gosec // G703: dir is the sandbox's own MkdirTemp result
		if err == nil {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: clearing read-only so removal can proceed; the tree is deleted next
		}
		return nil
	})
	if err := removeAll(dir); err != nil {
		return fmt.Errorf("sandbox: remove scratch directory: %w", err)
	}
	return nil
}

// withScratchEnv replaces a temp value equal to hostTemp; a different one was
// granted and stays. A cache variable is set only when absent, because the
// baseline never carries one. Windows names match case-insensitively. goos is a
// parameter so one test covers both rules.
func withScratchEnv(m map[string]string, scratch string, hostTemp func(string) string, goos string) {
	has := func(key string) bool {
		if goos != "windows" {
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
	for _, k := range scratchTempKeys(goos) {
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

// writableTargets creates the scratch directory, so a launch path that grants
// before building the environment finds it.
func (l *Local) writableTargets() ([]string, error) {
	scratch, err := l.scratchDir()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWriteGrant, err)
	}
	return append([]string{scratch}, l.writableDirs...), nil
}
