//go:build windows

package hardware

import (
	"context"
	"testing"
)

// TestSystemRAMBytesWindows checks the Win32 physical-memory probe answers with a
// plausible total. The call is the documented kernel API and needs no external tool, so a
// zero here means the probe is broken, not that the machine is unusual. The upper bound
// catches a misread of the MEMORYSTATUSEX layout, which would surface as a nonsense total
// rather than an error.
func TestSystemRAMBytesWindows(t *testing.T) {
	got := systemRAMBytes(context.Background())
	if got <= 0 {
		t.Fatalf("systemRAMBytes() = %d, want the machine's physical memory total", got)
	}
	const minPlausible = 256 << 20 // 256 MiB: below any machine that can run the agent
	const maxPlausible = 1 << 50   // 1 PiB: above any real machine, so a layout misread trips it
	if got < minPlausible || got > maxPlausible {
		t.Fatalf("systemRAMBytes() = %d bytes, outside the plausible range [%d, %d]", got, minPlausible, maxPlausible)
	}

	// The probe is a pure read of kernel state, so it must be stable across calls.
	if again := systemRAMBytes(context.Background()); again != got {
		t.Fatalf("systemRAMBytes() = %d then %d; the physical total must not move", got, again)
	}

	// A detected total is what makes a CPU-only run judgeable against real capacity.
	if !(Box{RAMBytes: got}).HasRAM() {
		t.Fatal("HasRAM() = false for a detected total")
	}
}
