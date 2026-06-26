//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
)

// init registers the Hyper-V microVM driver on Windows. Detection and boot are shared
// through commandDriver; this file supplies only how Windows reports its hardware boundary.
func init() { RegisterDriver(commandDriver{name: "hyperv", hardware: detectHyperV}) }

// detectHyperV reports whether the host has the Hyper-V platform installed, the Windows
// hardware virtualization boundary the strong tier runs on. It keys on the Hyper-V
// management service binary, which is installed only when the Hyper-V role is enabled, and
// not on the host compute service, which ships more broadly (for example for Windows
// containers) without the hypervisor. Detection is deliberately conservative: reporting the
// tier available where it is not would admit untrusted work onto a host that cannot contain
// it, so a missing signal reports unavailable rather than guessing yes.
func detectHyperV() Availability {
	sysDir := os.Getenv("SystemRoot")
	if sysDir == "" {
		sysDir = `C:\Windows`
	}
	const svc = `System32\vmms.exe`
	if fileExists(filepath.Join(sysDir, svc)) {
		return Availability{OK: true, Detail: "Hyper-V platform present (" + svc + ")"}
	}
	return Availability{Detail: "the Hyper-V platform is not installed (enable it to run untrusted guests)"}
}
