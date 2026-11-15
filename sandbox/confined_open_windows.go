//go:build windows

package sandbox

import (
	"os"

	"golang.org/x/sys/windows"
)

// confinedOpen uses FILE_FLAG_OPEN_REPARSE_POINT, the Windows O_NOFOLLOW, so a
// symlink swapped into the final component is opened as the link and refused,
// never followed or truncated through. A post-open re-validation catches an
// intermediate component redirected outside the root.
func (l *Local) confinedOpen(abs string, flags int, _ os.FileMode) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, err
	}

	var access uint32
	switch flags & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	default:
		access = windows.GENERIC_READ
	}

	var disposition uint32
	switch {
	case flags&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		disposition = windows.CREATE_NEW
	case flags&(os.O_CREATE|os.O_TRUNC) == os.O_CREATE|os.O_TRUNC:
		disposition = windows.CREATE_ALWAYS
	case flags&os.O_CREATE == os.O_CREATE:
		disposition = windows.OPEN_ALWAYS
	case flags&os.O_TRUNC == os.O_TRUNC:
		disposition = windows.TRUNCATE_EXISTING
	default:
		disposition = windows.OPEN_EXISTING
	}

	// Shared widely so a racing swap gives no spurious sharing violation; the
	// reparse-point refusal provides the safety.
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(p, access, share, nil, disposition,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: abs, Err: err}
	}

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, ErrDenied
	}

	return l.revalidateOpened(os.NewFile(uintptr(h), abs), abs)
}
