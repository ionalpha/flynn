//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// readDenyEnforceable is true because the command has its own mount namespace.
func readDenyEnforceable() bool { return true }

// hideUnreadable runs after every grant, so no grant restores a hidden path. Any
// failure fails the launch; a missing path is skipped.
func hideUnreadable(paths []string) error {
	for _, p := range paths {
		if err := hidePath(p); err != nil {
			return fmt.Errorf("hide %s: %w", p, err)
		}
	}
	return nil
}

// hidePath mounts an empty read-only tmpfs no one can enter over a directory, and
// /dev/null over anything else.
func hidePath(p string) error {
	info, err := os.Lstat(p) //nolint:gosec // G703: p names a path to hide; covering it only removes access, so a traversal in p cannot widen what the command reads
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		const flags = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_RDONLY
		return syscall.Mount("tmpfs", p, "tmpfs", flags, "mode=000,size=4k")
	}
	if err := syscall.Mount("/dev/null", p, "", syscall.MS_BIND, ""); err != nil {
		return err
	}
	return syscall.Mount("", p, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|keptMountFlags(p), "")
}
