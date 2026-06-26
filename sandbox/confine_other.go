//go:build !linux

package sandbox

import (
	"os/exec"

	"github.com/ionalpha/flynn/fault"
)

// denyNetwork reports that kernel-enforced network isolation is not available on this
// platform. It fails rather than running the command with the network still open, so
// a caller that asked for no network never silently gets one. The platform's native
// isolation (its kernel-confinement adapter) provides the equivalent where it lands.
func denyNetwork(_ *exec.Cmd) error {
	return fault.New(fault.Forbidden, "sandbox_netdeny_unsupported",
		"sandbox: network isolation is not supported on this platform yet; refusing rather than running with the network open")
}
