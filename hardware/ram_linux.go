//go:build linux

package hardware

import (
	"context"
	"os"
)

// systemRAMBytes reads the total system memory from /proc/meminfo, the kernel-exported
// source every Linux machine has. It is best-effort: an unreadable file or an
// unexpected format yields 0, treated by the caller as unknown.
func systemRAMBytes(_ context.Context) int64 {
	contents, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMeminfo(string(contents))
}
