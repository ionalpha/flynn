//go:build windows

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ACE header flags and types from winnt.h that x/sys/windows does not export.
const (
	aceObjectInherit      = 0x01
	aceContainerInherit   = 0x02
	aceNoPropagateInherit = 0x04
	aceInheritOnly        = 0x08
	aceInherited          = 0x10
)

// aclHasGrant reports whether acl holds an entry on the directory itself, not
// inherited from its parent, that allows sid every right in rights and passes to
// both files and subdirectories. Only such an entry means the grant ran, and ran
// with the inheritance that makes new files writable.
func aclHasGrant(acl *windows.ACL, sid *windows.SID, rights uint32) bool {
	if acl == nil {
		return false
	}
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		flags := ace.Header.AceFlags
		if flags&aceInherited != 0 || flags&(aceInheritOnly|aceNoPropagateInherit) != 0 {
			continue
		}
		if flags&(aceObjectInherit|aceContainerInherit) != aceObjectInherit|aceContainerInherit {
			continue
		}
		if uint32(ace.Mask)&rights != rights {
			continue
		}
		if windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), sid) {
			return true
		}
	}
	return false
}

// readDACL returns dir's access list.
func readDACL(dir string) (*windows.ACL, error) {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read access list: %w", err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("access list: %w", err)
	}
	return acl, nil
}

// grantMarkerDir is where completion markers live; a variable so a test points it
// at a temporary directory.
var grantMarkerDir = func() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "flynn", "sandbox-grants")
}

// grantMarkerPath names the file that records a finished grant of rights to sid on
// dir. The access list alone cannot say the grant finished: Windows sets the
// directory's own entry and then walks the tree, so a grant cancelled or killed
// midway leaves the entry on the directory and not on files not yet reached. A
// later launch that trusted the entry would run with files it cannot write.
func grantMarkerPath(dir string, sid *windows.SID, rights uint32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%x", strings.ToLower(filepath.Clean(dir)), sid.String(), rights)))
	return filepath.Join(grantMarkerDir(), hex.EncodeToString(sum[:16]))
}

func grantMarked(dir string, sid *windows.SID, rights uint32) bool {
	_, err := os.Stat(grantMarkerPath(dir, sid, rights))
	return err == nil
}

// markGrant is best effort: without the marker the next launch grants again,
// which is slow on a large tree and never wrong.
func markGrant(dir string, sid *windows.SID, rights uint32) {
	p := grantMarkerPath(dir, sid, rights)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(dir), 0o600)
}

// setTreeDACL applies acl to dir and propagates its inheritable entries to every
// descendant. A variable so a test counts the propagating writes.
var setTreeDACL = treeSetDACL

// ensureGrant gives sid rights on dir, inheritable, and does nothing when that is
// already in place. Setting the list rewrites the access list of every file under
// dir, which costs minutes on a large tree and cannot be interrupted once
// started, so it must not repeat: not on the next command (the Local remembers),
// not on the next Local for the same directory (the directory's own list and a
// completion marker say so), and not after Close, which leaves the entry in place.
//
// durable is false for a directory that exists for one Local, such as its scratch
// directory, so no marker outlives it.
func (l *Local) ensureGrant(ctx context.Context, dir string, sid *windows.SID, rights uint32, durable bool) error {
	key := fmt.Sprintf("%s|%s|%x", strings.ToLower(filepath.Clean(dir)), sid.String(), rights)
	if l.grants.has(key) {
		return nil
	}
	existing, err := readDACL(dir)
	if err != nil {
		return err
	}
	if durable && aclHasGrant(existing, sid, rights) && grantMarked(dir, sid, rights) {
		l.grants.mark(key)
		return nil
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(rights),
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, existing)
	if err != nil {
		return fmt.Errorf("merge access list: %w", err)
	}
	reportGrant(ctx, GrantProgress{Dir: dir})
	err = setTreeDACL(ctx, dir, merged)
	reportGrant(ctx, GrantProgress{Dir: dir, Done: true})
	if err != nil {
		return fmt.Errorf("apply access list: %w", err)
	}
	if durable {
		markGrant(dir, sid, rights)
	}
	l.grants.mark(key)
	return nil
}

// isScratch reports whether dir is this Local's scratch directory.
func (l *Local) isScratch(dir string) bool {
	l.scratch.mu.Lock()
	defer l.scratch.mu.Unlock()
	return l.scratch.dir != "" && strings.EqualFold(filepath.Clean(dir), filepath.Clean(l.scratch.dir))
}

var (
	procTreeSetNamedSecurityInfo = windows.NewLazySystemDLL("advapi32.dll").NewProc("TreeSetNamedSecurityInfoW")

	treeRuns   sync.Map // id -> *treeRun
	treeRunSeq atomic.Uintptr
)

// Values from accctrl.h.
const (
	treeSecInfoSet            = 1 // propagate the inheritable entries of the new list
	progressInvokeEveryObject = 2
	progressCancelOperation   = 4
)

// grantProgressEvery is how many rewritten objects pass between progress reports;
// a variable so a test sees every object.
var grantProgressEvery int64 = 200

// treeRun is one TreeSetNamedSecurityInfo call, found by the callback through the
// id passed as its argument.
type treeRun struct {
	ctx       context.Context
	dir       string
	files     atomic.Int64
	cancelled atomic.Bool
}

// treeProgress is the FN_PROGRESS callback. Windows calls it before and after each
// object, and setting the invoke setting to cancel stops the walk at the next one.
// One callback serves every call because a callback is never freed.
var treeProgress = windows.NewCallback(func(_ uintptr, _ uintptr, invoke *uint32, id uintptr, securitySet uintptr) uintptr {
	v, ok := treeRuns.Load(id)
	if !ok {
		return 0
	}
	run := v.(*treeRun)
	if run.ctx.Err() != nil {
		*invoke = progressCancelOperation
		run.cancelled.Store(true)
		return 0
	}
	if securitySet != 0 {
		n := run.files.Add(1)
		if n%grantProgressEvery == 0 {
			reportGrant(run.ctx, GrantProgress{Dir: run.dir, Files: n})
		}
	}
	return 0
})

// treeSetDACL is SetNamedSecurityInfo with a walk the caller can watch and stop.
// A cancelled walk has already set the directory's own entry and some of its
// descendants, which is why a grant is trusted only with its completion marker.
func treeSetDACL(ctx context.Context, dir string, acl *windows.ACL) error {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	run := &treeRun{ctx: ctx, dir: dir}
	id := treeRunSeq.Add(1)
	treeRuns.Store(id, run)
	defer treeRuns.Delete(id)

	r, _, _ := procTreeSetNamedSecurityInfo.Call(
		uintptr(unsafe.Pointer(name)),
		uintptr(windows.SE_FILE_OBJECT),
		uintptr(windows.DACL_SECURITY_INFORMATION),
		0, 0,
		uintptr(unsafe.Pointer(acl)),
		0,
		treeSecInfoSet,
		treeProgress,
		progressInvokeEveryObject,
		id,
	)
	if run.cancelled.Load() || ctx.Err() != nil {
		return ctx.Err()
	}
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}
