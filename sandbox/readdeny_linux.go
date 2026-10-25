//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// readDenyEnforceable reports whether this platform can hide a path from a confined
// command. Linux can: the command runs in a mount namespace of its own.
func readDenyEnforceable() bool { return true }

// hideUnreadable covers each path with an empty directory or an empty file in the
// launcher's mount namespace. It runs after every grant, so no grant restores a hidden
// path, and any failure fails the launch: a path that was to be hidden and is not
// must never reach the command. A path that does not exist is skipped.
func hideUnreadable(paths []string) error {
	for _, p := range paths {
		if err := hidePath(p); err != nil {
			return fmt.Errorf("hide %s: %w", p, err)
		}
	}
	return nil
}

// hidePath covers one path. A directory gets a read-only, empty tmpfs that no one can
// enter; anything else gets /dev/null bound over it.
func hidePath(p string) error {
	info, err := os.Lstat(p)
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
