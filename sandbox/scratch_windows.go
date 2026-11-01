//go:build windows

package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// scratchTempName is the scratch subdirectory TEMP and TMP point at. The caches sit beside
// it, not inside it, so a tool that empties its temp directory leaves the caches alone.
const scratchTempName = "tmp"

// scratchCaches maps the cache variables of toolchains that default under the user profile,
// where a confined child cannot write, to a subdirectory of the scratch directory. GOCACHE
// is what lets `go test` run at all. GOMODCACHE is redirected because the host's module
// cache is read-only to the child, and downloads then stay out of a cache that would outlive
// the sandbox. CARGO_HOME is left alone: it holds registry configuration and installed
// binaries, so redirecting it changes what cargo does, not only where it writes.
var scratchCaches = map[string]string{
	"GOCACHE":          "go-build",
	"GOMODCACHE":       "go-mod",
	"npm_config_cache": "npm",
	"PIP_CACHE_DIR":    "pip",
}

// scratchDir returns the sandbox's scratch directory, creating it on first use. It is the
// second writable location a confined child has besides the workspace: caches and temp
// files only, private to this sandbox, and deleted on Close. It lives outside the
// workspace so the workspace is never the temp root (Go ignores a go.mod found there) and
// so build output does not land in the user's project.
func (l *Local) scratchDir() (string, error) {
	l.scratch.mu.Lock()
	defer l.scratch.mu.Unlock()
	if l.scratch.dir != "" {
		return l.scratch.dir, nil
	}
	dir, err := os.MkdirTemp("", "flynn-scratch-")
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

// removeScratch deletes the scratch directory. Tools such as the Go module cache mark what
// they write read-only, which Windows will not delete, so a failed removal clears the
// attribute and tries again.
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

// withScratchEnv points TEMP, TMP and the toolchain cache variables at scratch. The host's
// own TEMP and TMP (hostTemp, which the baseline copies in) are replaced; a value that
// differs from the host's was granted by the operator and stays. A cache variable is set
// only when absent, since the baseline never carries one and so any present was granted.
// Windows treats variable names case-insensitively, so presence is checked that way too.
func withScratchEnv(m map[string]string, scratch string, hostTemp func(string) string) {
	has := func(key string) bool {
		for k := range m {
			if strings.EqualFold(k, key) {
				return true
			}
		}
		return false
	}
	for _, k := range []string{"TEMP", "TMP"} {
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
