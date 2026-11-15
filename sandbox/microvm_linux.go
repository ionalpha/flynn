//go:build linux

package sandbox

import "os"

// init registers the KVM microVM driver.
func init() { RegisterDriver(commandDriver{name: "kvm", hardware: detectKVM}) }

// detectKVM opens the device read-write, because only that confirms this user can
// create a guest.
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
