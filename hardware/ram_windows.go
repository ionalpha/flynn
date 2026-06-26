//go:build windows

package hardware

import (
	"context"
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX structure. Only ullTotalPhys is read;
// the rest are present so the layout and size match what the API expects.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var (
	modkernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatus = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// systemRAMBytes reads the physical memory total from the kernel via GlobalMemoryStatusEx,
// the documented Win32 call, so it needs no external tool and cannot be spoofed by PATH.
// It is best-effort: a failed call yields 0, treated by the caller as unknown.
func systemRAMBytes(_ context.Context) int64 {
	m := memoryStatusEx{}
	m.length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0
	}
	if m.totalPhys > 1<<62 {
		return 0 // implausible; refuse rather than overflow a signed budget
	}
	return int64(m.totalPhys)
}
