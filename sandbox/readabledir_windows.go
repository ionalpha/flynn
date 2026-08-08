//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// grantReadableDirs grants the AppContainer sid read and traverse access on each of the
// Local's extra readable directories, so a confined child can read files a run needs (an
// external CLI's auth or config home) that live outside the workspace. It is called on
// every confined launch after the workspace grant; the grant is idempotent (it replaces
// the sid's entry rather than stacking) and is removed on Close through revokeReadableDirs.
// A failure to grant fails the launch rather than starting a child that cannot read what
// it needs.
func (l *Local) grantReadableDirs(sid *windows.SID) error {
	for _, dir := range l.readableDirs {
		if err := grantReadDir(dir, sid); err != nil {
			// Wrapped in ErrReadGrant so the best-effort fallback refuses to absorb it: a
			// directory the host will not let this process grant (one whose access list it
			// cannot rewrite, or one that does not exist) must fail the launch, never drop
			// the child to an unconfined run.
			return fmt.Errorf("%w: %s: %w", ErrReadGrant, dir, err)
		}
	}
	return nil
}

// grantReadDir adds an inheritable read-and-execute entry for sid to dir's access list,
// merged with the existing list so the directory keeps its own access. Unlike grantDir
// (which grants full access to the one writable workspace), this grants only read and
// traverse: the child can read the directory's contents but cannot write there.
func grantReadDir(dir string, sid *windows.SID) error {
	return mergeAccessEntry(dir, windows.EXPLICIT_ACCESS{
		AccessPermissions: fileGenericReadExecute,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	})
}

// revokeReadableDirs removes the read grants added for this Local's container sid from
// each extra readable directory, so no access entry outlives the sandbox. It re-derives
// the deterministic per-workspace sid (the same one the launches granted) and is
// best-effort: the sid is unique to this ephemeral workspace, so a revoke that cannot run
// leaves at worst a dead, unresolvable entry, never a live access path.
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

// revokeDirAccess removes every access entry for sid from dir's access list, undoing the
// read grant grantReadDir added. It uses a REVOKE_ACCESS entry, which the merge resolves
// by dropping the trustee's existing entries; since sid is this sandbox's ephemeral
// container identity, that removes exactly the grant this Local added and nothing else.
func revokeDirAccess(dir string, sid *windows.SID) error {
	return mergeAccessEntry(dir, windows.EXPLICIT_ACCESS{
		AccessMode: windows.REVOKE_ACCESS,
		Trustee:    sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	})
}
