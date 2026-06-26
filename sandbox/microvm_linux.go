//go:build linux

package sandbox

import "os"

// init registers the KVM-backed microVM driver on Linux. Detection and boot are shared
// through commandDriver; this file supplies only how Linux exposes its hardware boundary.
func init() { RegisterDriver(commandDriver{name: "kvm", hardware: detectKVM}) }

// detectKVM reports whether the host exposes a usable KVM device, the Linux hardware
// virtualization boundary. Presence of the device is necessary; opening it read-write
// confirms the current user can actually create a guest, so a host where KVM exists but is
// inaccessible is reported unavailable rather than pretended ready.
func detectKVM() Availability {
	const dev = "/dev/kvm"
	if _, err := os.Stat(dev); err != nil {
		return Availability{Detail: "no KVM device at " + dev + " (host virtualization is off or unsupported)"}
	}
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return Availability{Detail: "KVM device present but not accessible: " + err.Error()}
	}
	_ = f.Close()
	return Availability{OK: true, Detail: "KVM available at " + dev}
}
