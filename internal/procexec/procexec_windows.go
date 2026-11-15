//go:build windows

package procexec

import "syscall"

// createNoWindow is CREATE_NO_WINDOW: a console child gets no console window.
// HideWindow is not used because it also passes SW_HIDE, which hides the main window
// of a GUI program the agent was asked to run.
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
