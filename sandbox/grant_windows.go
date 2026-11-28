//go:build windows

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/fsatomic"
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

func grantKey(dir string, sid *windows.SID, rights uint32) string {
	return fmt.Sprintf("%s|%s|%x", strings.ToLower(filepath.Clean(dir)), sid.String(), rights)
}

// grantMarkerPath names the file that records a finished grant of rights to sid on
// dir. The access list alone cannot say the grant finished: Windows sets the
// directory's own entry and then walks the tree, so a process that exits midway
// leaves the entry on the directory and not on files not yet reached. A later
// launch that trusted the entry would run with files it cannot write.
func grantMarkerPath(dir string, sid *windows.SID, rights uint32) string {
	sum := sha256.Sum256([]byte(grantKey(dir, sid, rights)))
	return filepath.Join(grantMarkerDir(), hex.EncodeToString(sum[:16]))
}

func grantMarked(dir string, sid *windows.SID, rights uint32) bool {
	_, err := os.Stat(grantMarkerPath(dir, sid, rights))
	return err == nil
}

// writeMarker is best effort: without the marker the next launch grants again,
// which is slow on a large tree and never wrong.
func writeMarker(marker, dir string) {
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return
	}
	_ = fsatomic.WriteFile(marker, []byte(dir), 0o600)
}

// setTreeDACL replaces dir's access list. Windows rewrites the inherited entries
// of everything under dir before the call returns, and the call cannot be
// interrupted. TreeSetNamedSecurityInfo can be, but it turns inheritance back on
// for a subdirectory that had it turned off, which would hand the sandbox a
// directory its owner had closed off. A variable so a test counts or delays it.
var setTreeDACL = func(dir string, acl *windows.ACL) error {
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// grantSlots holds one token per directory, keyed by lowercased path. Whoever has
// the token is the only one in this process rewriting that directory's list, so a
// second launch waits for a running grant and two merges cannot drop each other.
var grantSlots sync.Map

func grantSlot(dir string) chan struct{} {
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	v, _ := grantSlots.LoadOrStore(strings.ToLower(filepath.Clean(dir)), slot)
	return v.(chan struct{})
}

// grantReportEvery is how long a launch waits on a grant before first reporting
// it, and how often after that; a variable so a test need not wait.
var grantReportEvery = time.Second

// grantClock times the reports. A grant is a live wait on the host's filesystem
// and is never replayed.
var grantClock clock.Timing = clock.System{}

// grantWait times one launch's wait for a grant.
type grantWait struct {
	dir      string
	start    time.Time
	reported bool
}

func (w *grantWait) report(ctx context.Context, done bool) {
	reportGrant(ctx, GrantProgress{Dir: w.dir, Elapsed: grantClock.Now().Sub(w.start), Done: done})
}

// awaitGrant receives from ch, reporting the elapsed time while it waits, and
// gives up when ctx is done.
func awaitGrant[T any](ctx context.Context, w *grantWait, ch <-chan T) (T, error) {
	tick := grantClock.NewTimer(grantReportEvery)
	defer tick.Stop()
	for {
		select {
		case v := <-ch:
			return v, nil
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-tick.C():
			w.reported = true
			w.report(ctx, false)
			tick.Reset(grantReportEvery)
		}
	}
}

// ensureGrant gives sid rights on dir, inheritable, and does nothing when that is
// already in place. Granting rewrites the access list of every file under dir,
// which costs minutes on a large tree, so it must not repeat: not on the next
// command, not on the next Local for the same directory (the directory's own list
// and a completion marker say so), and not after Close.
//
// The write runs on its own goroutine, so a done ctx releases the caller at once.
// The write carries on while the process lives and records its marker when it
// finishes; a process that exits first leaves none, and the grant is redone.
//
// durable is false for a directory that exists for one Local, such as its scratch
// directory, so no marker outlives it.
func (l *Local) ensureGrant(ctx context.Context, dir string, sid *windows.SID, rights uint32, durable bool) error {
	if l.grants.has(grantKey(dir, sid, rights)) {
		return nil
	}
	w := &grantWait{dir: dir, start: grantClock.Now()}
	defer func() {
		if w.reported {
			w.report(ctx, true)
		}
	}()
	slot := grantSlot(dir)
	if _, err := awaitGrant(ctx, w, slot); err != nil {
		return err
	}
	write, err := l.planGrant(ctx, dir, sid, rights, durable)
	if err != nil || write == nil {
		slot <- struct{}{}
		return err
	}
	result := make(chan error, 1)
	go func() {
		err := write()
		slot <- struct{}{}
		result <- err
	}()
	failed, err := awaitGrant(ctx, w, result)
	if err != nil {
		return err
	}
	return failed
}

// planGrant returns the write that puts the grant in place, or a nil write when it
// already is. Everything that reads sid happens here, because the caller frees sid
// once it stops waiting. The caller holds dir's slot.
func (l *Local) planGrant(ctx context.Context, dir string, sid *windows.SID, rights uint32, durable bool) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := grantKey(dir, sid, rights)
	if l.grants.has(key) {
		return nil, nil
	}
	// Only the root and the scratch directory are remembered for the life of the
	// Local. Another Local's Close revokes the extra directories, so those are
	// read back every time.
	remember := func() {}
	if l.isScratch(dir) || strings.EqualFold(filepath.Clean(dir), filepath.Clean(l.root)) {
		remember = func() { l.grants.mark(key) }
	}
	existing, err := readDACL(dir)
	if err != nil {
		return nil, err
	}
	if durable && aclHasGrant(existing, sid, rights) && grantMarked(dir, sid, rights) {
		remember()
		return nil, nil
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.ACCESS_MASK(rights),
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           sidTrustee(sid, windows.TRUSTEE_IS_GROUP),
	}}, existing)
	if err != nil {
		return nil, fmt.Errorf("merge access list: %w", err)
	}
	marker := grantMarkerPath(dir, sid, rights)
	// A marker from an earlier grant must not vouch for this one if it is cut short.
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale grant marker: %w", err)
	}
	return func() error {
		if err := setTreeDACL(dir, merged); err != nil {
			return fmt.Errorf("apply access list: %w", err)
		}
		if durable {
			writeMarker(marker, dir)
		}
		remember()
		return nil
	}, nil
}

// isScratch reports whether dir is this Local's scratch directory.
func (l *Local) isScratch(dir string) bool {
	l.scratch.mu.Lock()
	defer l.scratch.mu.Unlock()
	return l.scratch.dir != "" && strings.EqualFold(filepath.Clean(dir), filepath.Clean(l.scratch.dir))
}
