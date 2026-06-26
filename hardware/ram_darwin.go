//go:build darwin

package hardware

import (
	"context"
	"os/exec"
	"time"
)

// systemRAMBytes queries the kernel for the physical memory total via sysctl, which is
// present on every macOS install and prints the byte count directly. It is
// best-effort: a missing tool or unexpected output yields 0. The context bounds the
// probe so a wedged tool cannot hang the caller.
func systemRAMBytes(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	return parseByteCount(string(out))
}
