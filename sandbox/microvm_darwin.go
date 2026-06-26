//go:build darwin

package sandbox

import (
	"os/exec"
	"strings"
)

// init registers the Hypervisor-framework microVM driver on macOS. Detection and boot are
// shared through commandDriver; this file supplies only how macOS reports its hardware
// boundary.
func init() { RegisterDriver(commandDriver{name: "hvf", hardware: detectHVF}) }

// detectHVF reports whether the host supports the Hypervisor framework, the macOS hardware
// virtualization boundary. The kernel advertises support through a sysctl; a value of 1
// means a guest can be created here.
func detectHVF() Availability {
	out, err := exec.Command("sysctl", "-n", "kern.hv_support").Output()
	if err != nil {
		return Availability{Detail: "could not query hypervisor support: " + err.Error()}
	}
	if strings.TrimSpace(string(out)) != "1" {
		return Availability{Detail: "the host does not support the hypervisor framework (kern.hv_support is not 1)"}
	}
	return Availability{OK: true, Detail: "hypervisor framework available (kern.hv_support=1)"}
}
