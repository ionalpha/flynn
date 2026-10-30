//go:build !windows

package procexec

import "os/exec"

// windowHidden is true where there is no console window to open.
func windowHidden(*exec.Cmd) bool { return true }
