package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ErrReadDeny reports a [WithUnreadablePaths] path that cannot be hidden: the
// platform cannot, the host filesystem is not confined, or a grant contains it.
// [WithBestEffortConfinement] never absorbs it, because running beside exposed
// credentials is the failure the option prevents.
var ErrReadDeny = errors.New("sandbox: cannot hide a configured path from the command")

// WithUnreadablePaths hides paths from a confined command, for a runner whose
// credentials sit beside what it launches. It requires [WithReadOnlyFS] and
// [ReadDenySupported]. On Linux a path reads as empty; on macOS access fails with
// a permission error, and a file created later stays hidden. On Windows, or when a
// path contains the working directory or a grant, the launch fails with
// [ErrReadDeny]. Paths are resolved once, so a second name (another bind mount)
// is not hidden.
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

// ReadDenySupported is true on Linux and macOS, so a caller can decide before a
// [WithUnreadablePaths] launch refuses.
func ReadDenySupported() bool { return readDenyEnforceable() }

// UnreadablePaths returns a copy, for a run record of what was hidden.
func (l *Local) UnreadablePaths() []string { return append([]string(nil), l.unreadable...) }

// guardLaunch refuses, before dispatch, confinement the platform lacks, egress or
// network denial it cannot enforce, and a read-deny set that cannot apply. Every
// launch path calls it first.
func (l *Local) guardLaunch() error {
	if err := l.guardUnsupported(); err != nil {
		return err
	}
	if err := l.guardEgress(); err != nil {
		return err
	}
	if err := l.guardNetworkDeny(); err != nil {
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

// keptVisible is the working directory and every granted directory.
func (l *Local) keptVisible() []string {
	kept := append([]string{l.root}, l.writableDirs...)
	return append(kept, l.readableDirs...)
}
