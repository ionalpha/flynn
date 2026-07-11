//go:build windows

package sandbox

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file holds the write-restricted confinement tier for Windows: the child runs
// with a write-restricted primary token, so it can read what the host user can but can
// write only where the workspace identity is explicitly granted. This is the Windows
// implementation of the cross-platform read-only-host contract (WithReadOnlyFS): on
// Linux and macOS the kernel tier already leaves the host readable and confines writes
// to the working directory, and this tier gives Windows the same semantics.
//
// It exists because the stronger AppContainer tier is deny-by-default for reads, and
// that extra strictness breaks a class of ordinary programs at a primitive level: the
// container token cannot query the drive-letter symbolic-link objects, so the
// NT-to-DOS mapping step of GetFinalPathNameByHandle(VOLUME_NAME_DOS) fails with
// access-denied. Any Rust program that calls std::fs::canonicalize (an external agent
// CLI among them) dies on it, and no filesystem grant can help because the denied
// object is not a file. A write-restricted token has no such gap: reads and object
// queries pass the ordinary user gate, while every write is additionally checked against
// the restricting identities (see createWriteRestrictedToken for why those are the
// workspace SID plus Everyone, and why this tier never modifies a shared object).

// Token restriction flags for CreateRestrictedToken.
const (
	// disableMaxPrivilege removes every privilege from the new token except
	// SeChangeNotifyPrivilege (bypass-traverse-checking, which ordinary path use needs).
	disableMaxPrivilege = 0x1
	// writeRestricted makes the restricting-SID gate apply to write access only:
	// a write is allowed only when both the ordinary user gate and an entry for one
	// of the restricting SIDs allow it, while reads check the user gate alone.
	writeRestricted = 0x8
)

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
)

// restrictSIDSubAuthorities is the number of variable sub-authorities in a workspace's
// restricting SID: the fixed service-account identifier plus four hash words.
const restrictSIDSubAuthorities = 5

// workspaceRestrictSID derives the restricting identity for a workspace root: a SID
// unique to that root, held by no principal on the host, that names "code confined to
// this workspace". It is the write gate's whole basis, so it has to be both unique per
// workspace (or one sandbox's child could write another's directory, since a workspace
// grants write to its own restricting SID) and stable across the launches of one
// sandbox (the grant is applied once and every launch must match it). Both come from
// hashing the root, exactly as the AppContainer moniker does.
//
// The SID is built under the NT-authority service-account identifier (S-1-5-80-*) with
// hash-derived sub-authorities, the same shape Windows itself uses for per-service
// identities. That namespace is used because a restricting SID may not be a package or
// capability SID (S-1-15-*): CreateRestrictedToken rejects those outright, so the
// AppContainer identity this workspace already has cannot serve as the restricting one.
// The value is never registered with the host and never grants anything by itself; it
// only ever appears as this token's restricting SID and as the trustee of the grant on
// the workspace directory.
func workspaceRestrictSID(root string) (*windows.SID, error) {
	sum := sha256.Sum256([]byte("flynn.sbx.restrict." + root))
	subs := [restrictSIDSubAuthorities]uint32{80} // SECURITY_SERVICE_ID_BASE_RID
	for i := range 4 {
		subs[i+1] = binary.LittleEndian.Uint32(sum[i*4 : i*4+4])
	}
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY, restrictSIDSubAuthorities,
		subs[0], subs[1], subs[2], subs[3], subs[4], 0, 0, 0, &sid,
	)
	if err != nil {
		return nil, fmt.Errorf("derive restricting sid: %w", err)
	}
	return sid, nil
}

// createWriteRestrictedToken builds a primary token that is a write-restricted copy of
// this process's token: same user and groups for reads, all privileges dropped, and
// writes allowed only where one of the restricting SIDs is granted. The caller owns the
// returned token and closes it after the child is created (the child holds its own
// reference).
//
// The restricting set is three SIDs: the workspace's own restricting SID, the caller's
// logon-session SID, and Everyone. The restricting check is satisfied when the target
// object grants access to ANY of them, and each is load-bearing for a different reason:
//
//   - The workspace SID is what the workspace directory grants (and nothing else does), so
//     it is the write path into the one writable location and nothing outside it.
//   - The logon-session SID is what the interactive window station and desktop grant. A
//     process writes to those while user32 initializes; a restricted token whose set omits
//     the logon SID cannot, and the process dies during DLL initialization with
//     STATUS_DLL_INIT_FAILED (0xC0000142) before it reaches main.
//   - Everyone is what the session's base named objects grant, which a starting process
//     also writes. Omitting it fails startup the same way even when the logon SID is
//     present (proven: a {workspace, logon} set without Everyone still dies 0xC0000142).
//
// The two startup SIDs let a freshly restricted process complete its startup writes
// WITHOUT the sandbox touching any shared object's security. That distinction matters:
// modifying the interactive window station's access list to admit a private SID can strip
// the interactive user's own access and wedge every later process in the logon session, so
// this tier never modifies a shared object; it only widens the token's own restricting set
// to identities those objects already grant.
//
// Neither startup SID widens the file write gate in practice. Host files grant the user
// account SID, not Everyone and not the logon SID, so they stay unwritable (proven: codex
// under this token cannot write its own credential home). Logon SIDs appear on window
// stations and desktops, not on filesystem paths, so they open no file. The residual write
// surface is the handful of world-writable locations Windows ships (shared temp, the
// named-object namespace), which the workspace confinement was never the boundary for
// anyway (egress and the otherwise-read-only host are). The workspace stays isolated
// because it grants the per-workspace SID alone.
func createWriteRestrictedToken(workspaceSID *windows.SID) (windows.Token, error) {
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return 0, fmt.Errorf("everyone sid: %w", err)
	}

	var base windows.Token
	err = windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY, &base)
	if err != nil {
		return 0, fmt.Errorf("open process token: %w", err)
	}
	defer func() { _ = base.Close() }()

	logon, err := logonSessionSID(base)
	if err != nil {
		return 0, fmt.Errorf("logon session sid: %w", err)
	}

	restrict := []windows.SIDAndAttributes{{Sid: workspaceSID}, {Sid: logon}, {Sid: everyone}}
	var restricted windows.Token
	r, _, e := procCreateRestrictedToken.Call(
		uintptr(base),
		uintptr(writeRestricted|disableMaxPrivilege),
		0, 0, // no SIDs disabled
		0, 0, // no privileges beyond disableMaxPrivilege's sweep
		uintptr(len(restrict)), uintptr(unsafe.Pointer(&restrict[0])),
		uintptr(unsafe.Pointer(&restricted)),
	)
	if r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", e)
	}
	return restricted, nil
}

// logonSessionSID returns the logon-session SID from a token: the group carrying the
// SE_GROUP_LOGON_ID attribute, a per-logon identity the interactive window station and
// desktop grant access to. It is read from the base token rather than fabricated because
// it is specific to this process's logon session, and a restricted child must present the
// same one to open the station it inherits. The returned SID is owned by the garbage
// collector (a copy), so the caller does not free it.
func logonSessionSID(token windows.Token) (*windows.SID, error) {
	groups, err := token.GetTokenGroups()
	if err != nil {
		return nil, fmt.Errorf("token groups: %w", err)
	}
	for _, g := range groups.AllGroups() {
		if g.Attributes&windows.SE_GROUP_LOGON_ID != 0 {
			return g.Sid.Copy()
		}
	}
	return nil, errors.New("no logon-session sid in token")
}

// spawnWriteRestricted creates and starts a command under the write-restricted token
// for the workspace identified by root. The restricting SID is the workspace's own (see
// workspaceRestrictSID), and the grant that names it on the workspace directory (see
// grantRestrictedDir) is what opens the one writable location: a write anywhere that SID
// is not granted fails the restricting gate regardless of the user's own access. The
// child otherwise gets the same envelope as the container tier: only the launch pipes
// inherited, created suspended into its job, mitigation policies applied at creation.
func spawnWriteRestricted(appName, cmdline, root string, env *uint16, io confinedIO, resLimits ResourceLimits) (*acProcess, error) {
	sid, err := workspaceRestrictSID(root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	defer func() { _ = windows.FreeSid(sid) }()
	token, err := createWriteRestrictedToken(sid)
	if err != nil {
		return nil, fmt.Errorf("sandbox: restricted token: %w", err)
	}
	defer func() { _ = token.Close() }() // the child holds its own reference once created
	return spawnConfined(appName, cmdline, root, env, nil, token, io, resLimits)
}

// grantRestrictedDir grants the workspace's restricting SID full access to dir, the
// single write the write-restricted tier permits. Under a write-restricted token every
// write is checked twice, against the user's own access and against the restricting SID,
// so a directory without this entry is unwritable to the child even though the user can
// write it. The entry is inheritable, so files and directories the child creates stay
// writable to it.
func grantRestrictedDir(dir, root string) error {
	sid, err := workspaceRestrictSID(root)
	if err != nil {
		return err
	}
	defer func() { _ = windows.FreeSid(sid) }()
	return grantDir(dir, sid)
}

// revokeRestrictedDir removes the workspace's restricting-SID grant from dir, so no
// access entry outlives the sandbox. It is best-effort for the same reason the container
// tier's revoke is: the SID is unique to this workspace and held by no principal, so an
// entry left behind grants nothing to anyone.
func revokeRestrictedDir(dir, root string) error {
	sid, err := workspaceRestrictSID(root)
	if err != nil {
		return err
	}
	defer func() { _ = windows.FreeSid(sid) }()
	return revokeDirAccess(dir, sid)
}
