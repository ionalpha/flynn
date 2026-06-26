//go:build !linux && !darwin && !windows

package hardware

import "context"

// systemRAMBytes has no probe on this platform, so the total stays unknown and a caller
// falls back to an explicit budget.
func systemRAMBytes(_ context.Context) int64 { return 0 }
