package hardware

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// FreeVRAM reports how much memory of the first NVIDIA GPU is unused right now. The
// desktop compositor and other programs already hold part of the card, so a model sized to
// [Box.VRAMBytes] can still fail to load; this is the figure a fit is judged against. It
// reports false when the vendor tool is absent or prints nothing usable, and the caller
// then falls back to the total.
func FreeVRAM(ctx context.Context) (int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := runCommand(ctx, "nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits")
	if err != nil {
		return 0, false
	}
	return parseFreeVRAM(string(out))
}

// parseFreeVRAM reads the first row of the memory.free query, a count of mebibytes. A
// negative or non-numeric row ("N/A" on a driver that hides the figure) is unusable, and
// is not skipped in favour of a later GPU's row, which would describe a different card.
func parseFreeVRAM(out string) (int64, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		mib, err := strconv.ParseInt(line, 10, 64)
		if err != nil || mib < 0 {
			return 0, false
		}
		return mib * 1024 * 1024, true
	}
	return 0, false
}
