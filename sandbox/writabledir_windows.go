//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// grantWritableDirs grants the AppContainer sid full access on each of the Local's extra
// writable directories, so a confined child can persist state a run needs (an external
// CLI's refreshed credential or session log) outside the workspace. It is the write
// counterpart of grantReadableDirs and is called on every confined launch of the
// container tier, after the workspace grant. A failure to grant fails the launch rather
// than starting a child whose writes will silently fail partway through an episode.
func (l *Local) grantWritableDirs(sid *windows.SID) error {
	for _, dir := range l.writableDirs {
		if err := grantDir(dir, sid); err != nil {
			// Wrapped in ErrWriteGrant so the best-effort fallback refuses to absorb it, for
			// the same reason the read grant refuses: a directory this process cannot grant
			// must fail the launch, never drop the child to an unconfined run in which it
			// could write the whole host rather than this one directory.
			return fmt.Errorf("%w: %s: %w", ErrWriteGrant, dir, err)
		}
	}
	return nil
}

// grantRestrictedWritableDirs grants the workspace's restricting SID write access on each
// extra writable directory under the write-restricted tier. That tier gates writes on the
// restricting SID rather than on a container identity, so a directory becomes writable to
// the child exactly when it carries an entry for that SID, which is what the workspace
// grant already does for the root (see restricted_windows.go).
func (l *Local) grantRestrictedWritableDirs() error {
	for _, dir := range l.writableDirs {
		if err := grantRestrictedDir(dir, l.root); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrWriteGrant, dir, err)
		}
	}
	return nil
}

// revokeWritableDirs removes the write grants this Local added from each extra writable
// directory, so no access entry outlives the sandbox. It re-derives whichever identity
// the Local's tier granted (the workspace restricting SID for the write-restricted tier,
// the deterministic container SID otherwise) and is best-effort for the same reason the
// read revoke is: both SIDs are unique to this workspace, so an entry left behind is a
// dead one rather than a live access path.
func (l *Local) revokeWritableDirs() {
	if len(l.writableDirs) == 0 {
		return
	}
	if l.hostReadable {
		for _, dir := range l.writableDirs {
			_ = revokeRestrictedDir(dir, l.root)
		}
		return
	}
	sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
	if err != nil {
		return
	}
	defer func() { _ = windows.FreeSid(sid) }()
	for _, dir := range l.writableDirs {
		_ = revokeDirAccess(dir, sid)
	}
}
