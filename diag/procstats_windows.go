//go:build windows

package diag

import (
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetProcessHandleCt = kernel32.NewProc("GetProcessHandleCount")
)

// openFDs counts the process's open kernel handles with GetProcessHandleCount, which
// needs no system snapshot. There is no childProcs: a CreateToolhelp32Snapshot every
// sample costs a pass over every process, and a reused parent pid miscounts strangers.
// Config.Children supplies the count.
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
