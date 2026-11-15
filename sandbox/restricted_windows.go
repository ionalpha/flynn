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

// The write-restricted tier is the Windows WithReadOnlyFS: the child reads what
// the user can and writes only where the workspace SID is granted.
//
// AppContainer cannot serve here because its token cannot query the drive-letter
// symbolic links, so GetFinalPathNameByHandle(VOLUME_NAME_DOS) is access-denied
// and any Rust std::fs::canonicalize dies. No file grant helps, because the
// denied object is not a file. A write-restricted token checks reads against the
// user alone.

// CreateRestrictedToken flags.
const (
	// disableMaxPrivilege keeps only SeChangeNotifyPrivilege, which path traversal
	// needs.
	disableMaxPrivilege = 0x1
	// writeRestricted checks only writes against the restricting SIDs, on top of
	// the user gate.
	writeRestricted = 0x8
)

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
)

// restrictSIDSubAuthorities is the service-account identifier plus four hash
// words.
const restrictSIDSubAuthorities = 5

// workspaceRestrictSID hashes the root into an S-1-5-80-* SID that no principal
// holds. It must be unique per workspace, or one child could write another's
// directory, and stable across launches, because the grant is applied once.
// CreateRestrictedToken rejects S-1-15-* SIDs, so the AppContainer SID cannot
// serve.
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

// createWriteRestrictedToken drops every privilege and lets a write pass where the
// object grants the workspace SID, the logon SID or Everyone. The caller closes it
// once the child exists.
//
// user32 startup writes the window station and desktop, which grant the logon SID,
// and base named objects, which grant Everyone. Without either the child dies with
// STATUS_DLL_INIT_FAILED (0xC0000142) before main; {workspace, logon} alone fails
// too. The tier never edits a shared object's DACL, because admitting a private SID
// to the window station can strip the user's access and wedge the logon session.
//
// Host files grant the user SID, so they stay unwritable: codex under this token
// cannot write its credential home. Logon SIDs open no file. What stays writable is
// the world-writable shared temp and named-object namespace, which egress control
// and the read-only host cover.
func createWriteRestrictedToken(workspaceSID *windows.SID) (windows.Token, error) {
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return 0, fmt.Errorf("everyone sid: %w", err)
	}

	var base windows.Token
	err = windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT, &base)
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
	if err := grantDefaultDacl(restricted, workspaceSID); err != nil {
		_ = restricted.Close()
		return 0, err
	}
	return restricted, nil
}

// grantDefaultDacl adds the workspace SID to the default DACL stamped on the
// child's pipes, events and sections. The inherited one names only the user,
// SYSTEM and administrators, so the child's own pipe refuses its write end:
// os.Pipe fails "pipe: Access is denied" and `go build` stops.
func grantDefaultDacl(token windows.Token, workspaceSID *windows.SID) error {
	var n uint32
	_ = windows.GetTokenInformation(token, windows.TokenDefaultDacl, nil, 0, &n)
	if n == 0 {
		return errors.New("sandbox: size token default access list")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(token, windows.TokenDefaultDacl, &buf[0], n, &n); err != nil {
		return fmt.Errorf("sandbox: read token default access list: %w", err)
	}
	existing := (*struct{ DefaultDacl *windows.ACL })(unsafe.Pointer(&buf[0])).DefaultDacl
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee:           sidTrustee(workspaceSID, windows.TRUSTEE_IS_GROUP),
	}}, existing)
	if err != nil {
		return fmt.Errorf("sandbox: merge token default access list: %w", err)
	}
	info := struct{ DefaultDacl *windows.ACL }{merged}
	if err := windows.SetTokenInformation(token, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("sandbox: set token default access list: %w", err)
	}
	return nil
}

// logonSessionSID returns the SE_GROUP_LOGON_ID group, read from the token because
// the child must present this session's SID to open the station it inherits. The
// result is a garbage-collected copy, so the caller does not free it.
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

// spawnWriteRestricted gives the child the container tier's envelope: only the
// launch pipes inherited, created suspended into its job, and mitigation policies
// applied at creation.
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

// grantRestrictedDir is the tier's only write grant. It is inheritable, so what
// the child creates stays writable to it.
func grantRestrictedDir(dir, root string) error {
	sid, err := workspaceRestrictSID(root)
	if err != nil {
		return err
	}
	defer func() { _ = windows.FreeSid(sid) }()
	return grantDir(dir, sid)
}

// revokeRestrictedDir is best-effort because a leftover entry names a SID no
// principal holds.
func revokeRestrictedDir(dir, root string) error {
	sid, err := workspaceRestrictSID(root)
	if err != nil {
		return err
	}
	defer func() { _ = windows.FreeSid(sid) }()
	return revokeDirAccess(dir, sid)
}
