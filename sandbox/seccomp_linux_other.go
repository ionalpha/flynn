//go:build linux && !amd64 && !arm64

package sandbox

import "fmt"

// installSeccomp refuses on a Linux architecture without its own syscall filter. The
// denied-syscall numbers are architecture-specific, so a filter is only correct on an
// architecture it was written for (amd64, arm64). Rather than run a command with the
// wrong filter or none at all, confinement fails closed here, matching the rule that a
// requested guarantee is refused, never silently downgraded.
func installSeccomp() error {
	return fmt.Errorf("sandbox: seccomp confinement is not available on this architecture")
}
