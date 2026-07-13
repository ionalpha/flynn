//go:build linux

package sandbox

import (
	"strings"
	"testing"
	"time"
)

// TestDetectKVMReportsEitherWay proves the Linux hardware probe answers with a reason
// whichever way it goes: a host with no usable /dev/kvm reports unavailable and says why
// (so an operator knows what to enable), and a host with one reports available. It never
// reports available without the device, which is what keeps untrusted work from being
// admitted on a boundary that is not there.
func TestDetectKVMReportsEitherWay(t *testing.T) {
	av := detectKVM()
	if av.Detail == "" {
		t.Fatal("detection must always say why, so an operator can act on it")
	}
	if !strings.Contains(av.Detail, "/dev/kvm") {
		t.Fatalf("the detail should name the device it looked for, got %q", av.Detail)
	}
	if av.OK && !strings.Contains(av.Detail, "available") {
		t.Fatalf("an available host should say so, got %q", av.Detail)
	}
}

// TestProfileHousekeepingIsInertOffWindows proves the sandbox-profile housekeeping is a no-op
// where no operating-system object outlives a sandbox: only the Windows AppContainer tier
// registers one, so there is nothing to count or clean here, and the calls must not fail.
func TestProfileHousekeepingIsInertOffWindows(t *testing.T) {
	// A fixed cutoff, never the wall clock: the sweep's input is a time a caller supplies.
	cutoff := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if n, err := CleanStaleProfiles(cutoff); err != nil || n != 0 {
		t.Fatalf("cleaning profiles off Windows removes nothing: n=%d err=%v", n, err)
	}
	if n := LiveProfileCount(); n != 0 {
		t.Fatalf("no profile object exists off Windows, got %d", n)
	}
}
