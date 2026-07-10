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

// openFDs counts the process's open kernel handles, which is what a descriptor is
// on Windows. GetProcessHandleCount answers for the current process without a
// snapshot of the whole system.
//
// That "without a snapshot" is why there is deliberately no childProcs here. Counting
// children meant a CreateToolhelp32Snapshot of every process on the machine, on every
// sample, and it answered the wrong question: a snapshot entry can name a parent pid
// that has already exited and been reused, so the count included processes that merely
// claim this pid as a parent. The count comes from the spawners instead, through
// Config.Children.
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
