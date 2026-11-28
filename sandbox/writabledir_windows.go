//go:build windows

package sandbox

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows"
)

// grantWritableDirs gives sid full access on each extra writable directory, such
// as a CLI's credential store, after the workspace grant. A failure fails the
// launch so writes do not fail partway through an episode.
func (l *Local) grantWritableDirs(ctx context.Context, sid *windows.SID) error {
	dirs, err := l.writableTargets()
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := l.grantDir(ctx, dir, sid); err != nil {
			// ErrWriteGrant so the best-effort fallback never runs the child unconfined,
			// where it could write the whole host.
			return fmt.Errorf("%w: %s: %w", ErrWriteGrant, dir, err)
		}
	}
	return nil
}

// grantRestrictedWritableDirs serves the write-restricted tier, which gates writes
// on the restricting SID, so a directory is writable exactly when it has an entry
// for that SID.
func (l *Local) grantRestrictedWritableDirs(ctx context.Context) error {
	dirs, err := l.writableTargets()
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := l.grantRestrictedDir(ctx, dir); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrWriteGrant, dir, err)
		}
	}
	return nil
}

// revokeWritableDirs re-derives the SID the tier granted (restricting or container)
// and is best-effort because either SID is unique to this workspace, so a missed
// revoke leaves a dead entry.
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
