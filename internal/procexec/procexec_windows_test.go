//go:build windows

package procexec

import (
	"os/exec"
	"syscall"
	"testing"
)

// windowHidden reports whether c asks Windows for no console window.
func windowHidden(c *exec.Cmd) bool {
	return c.SysProcAttr != nil && c.SysProcAttr.CreationFlags&createNoWindow != 0
}

// TestHideWindowKeepsTheCallersFlags guards the merge: a caller that already chose creation
// flags or a token must keep them when the window flag is added.
func TestHideWindowKeepsTheCallersFlags(t *testing.T) {
	const newProcessGroup = 0x00000200
	in := &syscall.SysProcAttr{CreationFlags: newProcessGroup, HideWindow: true}
	out := hideWindow(in)
	if out.CreationFlags != newProcessGroup|createNoWindow || !out.HideWindow {
		t.Fatalf("flags = %#x hide=%v, want the caller's flags kept and CREATE_NO_WINDOW added", out.CreationFlags, out.HideWindow)
	}
	if again := hideWindow(out); again.CreationFlags != out.CreationFlags {
		t.Fatalf("hiding twice changed the flags: %#x then %#x", out.CreationFlags, again.CreationFlags)
	}
}
