//go:build windows

package diag

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetProcessHandleCt = kernel32.NewProc("GetProcessHandleCount")
)

// openFDs counts the process's open kernel handles, which is what a descriptor is
// on Windows. GetProcessHandleCount answers for the current process without a
// snapshot of the whole system.
func openFDs() int {
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return Unknown
	}
	var count uint32
	r, _, _ := procGetProcessHandleCt.Call(uintptr(self), uintptr(unsafe.Pointer(&count)))
	if r == 0 {
		return Unknown
	}
	return int(count)
}

// childProcs counts live processes whose parent is this one, by walking a
// process snapshot. Windows does not reparent orphans, so a snapshot entry can name
// a parent pid that has already exited and been reused; the count is therefore of
// processes that currently claim this pid as parent, which is exactly the set an
// unreaped sandboxed command lands in.
func childProcs() int {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return Unknown
	}
	defer func() { _ = syscall.CloseHandle(snapshot) }()

	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := syscall.Process32First(snapshot, &entry); err != nil {
		return Unknown
	}

	self := uint32(os.Getpid()) //nolint:gosec // a pid fits a DWORD; that is the type Windows gave it
	count := 0
	for {
		if entry.ParentProcessID == self {
			count++
		}
		if err := syscall.Process32Next(snapshot, &entry); err != nil {
			break // ERROR_NO_MORE_FILES ends the walk
		}
	}
	return count
}
