//go:build !windows

package procexec

import "syscall"

// hideWindow returns attr unchanged: only Windows gives a child a console window of its own.
func hideWindow(attr *syscall.SysProcAttr) *syscall.SysProcAttr { return attr }
