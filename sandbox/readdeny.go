package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ErrReadDeny reports that a path named by [WithUnreadablePaths] cannot be hidden from the
// confined command: the platform has no way to hide it, the Local does not confine the
// host's filesystem, or the set contradicts the grants. It fails the launch and is never absorbed
// by [WithBestEffortConfinement]: a command that was to be kept away from a runner's
// credentials and ran without that is the failure the option exists to prevent.
var ErrReadDeny = errors.New("sandbox: cannot hide a configured path from the command")

// WithUnreadablePaths hides files and directories from a confined command, for a runner
// whose data directory, control socket and credential files sit beside the commands it
// launches; the default confinement leaves the host readable.
//
// It requires [WithReadOnlyFS] and a platform that can hide a path ([ReadDenySupported]).
// Linux covers each path in the command's own mount view, so it reads as empty; macOS
// denies every access under it, so a read fails with a permission error and a file created
// there later stays hidden. On Windows, or when a path contains the working directory or a
// grant, the launch fails with [ErrReadDeny]. Paths are made absolute and symlink-free
// once; a path reachable under a second name (another bind mount) is not hidden under it.
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

// ReadDenySupported reports whether this platform can hide a path from a command it
// confines, which is whether [WithUnreadablePaths] can launch at all. It is true on Linux
// and macOS and false on Windows, so a caller can choose what to do there before a launch
// refuses.
func ReadDenySupported() bool { return readDenyEnforceable() }

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
		return fmt.Errorf("%w: this platform cannot hide a path from a command it confines; use the container or microVM tier", ErrReadDeny)
	}
	if !l.readonlyFS {
		return fmt.Errorf("%w: a Local without WithReadOnlyFS does not confine the host's filesystem", ErrReadDeny)
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
