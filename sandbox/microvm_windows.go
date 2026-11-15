//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
)

// init registers the Hyper-V driver; commandDriver shares detection and boot.
func init() { RegisterDriver(commandDriver{name: "hyperv", hardware: detectHyperV}) }

// detectHyperV keys on the Hyper-V management service binary, present only with
// the Hyper-V role. The host compute service also ships for Windows containers
// without the hypervisor. A missing signal reports unavailable, so untrusted work
// never lands on a host that cannot contain it.
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
