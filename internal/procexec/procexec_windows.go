//go:build windows

package procexec

import "syscall"

// createNoWindow is the CREATE_NO_WINDOW process creation flag: a console child is given no
// console window. It is the flag alone, not HideWindow, because HideWindow also passes
// SW_HIDE to the child's startup info, which hides the main window of a GUI program the
// agent was asked to run, a different behaviour from the one this package exists for.
const createNoWindow = 0x08000000

// hideWindow returns attr with createNoWindow set, allocating it when nil and keeping every
// other field and flag the caller set.
func hideWindow(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.CreationFlags |= createNoWindow
	return attr
}
