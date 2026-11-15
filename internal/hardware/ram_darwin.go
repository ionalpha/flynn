//go:build darwin

package hardware

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/internal/procexec"
)

// systemRAMBytes queries sysctl, present on every macOS install, for the physical memory
// total in bytes. A missing tool or unexpected output yields 0, and the context bounds the
// probe so a wedged tool cannot hang the caller.
func systemRAMBytes(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := procexec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	return parseByteCount(string(out))
}
