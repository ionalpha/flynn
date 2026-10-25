package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ErrReadDeny reports that a path named by [WithUnreadablePaths] cannot be hidden from the
// confined command: the platform has no way to hide it, the Local does not build a private
// mount view, or the set contradicts the grants. It fails the launch and is never absorbed
// by [WithBestEffortConfinement]: a command that was to be kept away from a runner's
// credentials and ran without that is the failure the option exists to prevent.
var ErrReadDeny = errors.New("sandbox: cannot hide a configured path from the command")

// WithUnreadablePaths hides files and directories from a confined command. The default
// confinement leaves the host readable, which suits a development checkout and does not
// suit a runner whose data directory, control socket and credential files sit beside the
// commands it launches. A directory is replaced by an empty one and a file by an empty
// file, so the command sees that the path exists and finds nothing in it.
//
// It requires [WithReadOnlyFS], which is what gives the command a mount view of its own,
// and a platform that builds one (Linux). Anywhere else, and when a path contains the
// working directory or a granted directory, the launch fails with [ErrReadDeny]. A path
// that does not exist when the command starts is skipped, since there is nothing to read.
// Paths are made absolute and symlink-free once, like the root. A path reachable under a
// second name, by another bind mount of the same files, is not hidden under that name.
func WithUnreadablePaths(paths ...string) LocalOption {
	return func(l *Local) {
		for _, p := range paths {
			if p == "" {
				continue
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				abs = resolved
			}
			l.unreadable = append(l.unreadable, abs)
		}
	}
}

// UnreadablePaths lists the paths hidden from the commands this Local confines, so a
// record of the run can show what the command was kept away from.
func (l *Local) UnreadablePaths() []string { return append([]string(nil), l.unreadable...) }

// guardLaunch refuses a launch the Local cannot honour before any dispatch: confinement
// the platform lacks, governed egress without an enforcement leg, and a read-deny set that
// cannot be applied. Every launch path calls it first, so none has a check of its own.
func (l *Local) guardLaunch() error {
	if err := l.guardUnsupported(); err != nil {
		return err
	}
	if err := l.guardEgress(); err != nil {
		return err
	}
	return l.guardReadDeny()
}

// guardReadDeny refuses a launch whose read-deny set would not hold.
func (l *Local) guardReadDeny() error {
	if len(l.unreadable) == 0 {
		return nil
	}
	if !readDenyEnforceable() {
		return fmt.Errorf("%w: this platform builds no private mount view; use the container or microVM tier", ErrReadDeny)
	}
	if !l.readonlyFS {
		return fmt.Errorf("%w: a Local without WithReadOnlyFS shares the host's mount view", ErrReadDeny)
	}
	for _, hidden := range l.unreadable {
		for _, kept := range l.keptVisible() {
			if pathWithin(kept, hidden) {
				return fmt.Errorf("%w: %s lies under %s", ErrReadDeny, kept, hidden)
			}
		}
	}
	return nil
}

// keptVisible lists the paths the command is meant to use: its working directory and every
// directory granted to it.
func (l *Local) keptVisible() []string {
	kept := append([]string{l.root}, l.writableDirs...)
	return append(kept, l.readableDirs...)
}
