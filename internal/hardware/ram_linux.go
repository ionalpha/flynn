//go:build linux

package hardware

import (
	"context"
	"os"
)

// systemRAMBytes reads the total from /proc/meminfo. An unreadable file or unexpected
// format yields 0, read by the caller as unknown.
func systemRAMBytes(_ context.Context) int64 {
	contents, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMeminfo(string(contents))
}
