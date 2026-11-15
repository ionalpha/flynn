//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// grantReadableDirs gives sid read and traverse on each extra readable directory,
// such as a CLI's config home. It runs on every confined launch after the
// workspace grant, and is idempotent because it replaces sid's entry. A failure
// fails the launch.
func (l *Local) grantReadableDirs(sid *windows.SID) error {
	for _, dir := range l.readableDirs {
		if err := grantReadDir(dir, sid); err != nil {
			// ErrReadGrant so the best-effort fallback never runs the child unconfined.
			return fmt.Errorf("%w: %s: %w", ErrReadGrant, dir, err)
		}
	}
	return nil
}

// grantReadDir merges an inheritable read-and-execute entry into dir's list, so
// the directory keeps its own access and the child cannot write there.
func grantReadDir(dir string, sid *windows.SID) error {
	return mergeAccessEntry(dir, windows.EXPLICIT_ACCESS{
		AccessPermissions: fileGenericReadExecute,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	})
}

// revokeReadableDirs re-derives the per-workspace sid and removes its grants. It
// is best-effort because the sid is unique to this workspace, so a missed revoke
// leaves a dead entry.
func (l *Local) revokeReadableDirs() {
	if len(l.readableDirs) == 0 {
		return
	}
	sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
	if err != nil {
		return
	}
	defer func() { _ = windows.FreeSid(sid) }()
	for _, dir := range l.readableDirs {
		_ = revokeDirAccess(dir, sid)
	}
}

// revokeDirAccess merges a REVOKE_ACCESS entry, which drops every entry for sid
// and, because sid is this sandbox's own, nothing else.
func revokeDirAccess(dir string, sid *windows.SID) error {
	return mergeAccessEntry(dir, windows.EXPLICIT_ACCESS{
		AccessMode: windows.REVOKE_ACCESS,
		Trustee:    sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	})
}
