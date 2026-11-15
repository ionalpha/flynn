//go:build linux && !amd64 && !arm64

package sandbox

import "fmt"

// installSeccomp refuses, because syscall numbers are per architecture and only
// amd64 and arm64 have a filter.
func installSeccomp() error {
	return fmt.Errorf("sandbox: seccomp confinement is not available on this architecture")
}
