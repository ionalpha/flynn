//go:build darwin

package sandbox

import (
	"strings"

	"github.com/ionalpha/flynn/internal/procexec"
)

// init registers the Hypervisor-framework microVM driver.
func init() { RegisterDriver(commandDriver{name: "hvf", hardware: detectHVF}) }

// detectHVF reads a sysctl, where 1 means a guest can be created.
func detectHVF() Availability {
	out, err := procexec.Command("sysctl", "-n", "kern.hv_support").Output()
	if err != nil {
		return Availability{Detail: "could not query hypervisor support: " + err.Error()}
	}
	if strings.TrimSpace(string(out)) != "1" {
		return Availability{Detail: "the host does not support the hypervisor framework (kern.hv_support is not 1)"}
	}
	return Availability{OK: true, Detail: "hypervisor framework available (kern.hv_support=1)"}
}
