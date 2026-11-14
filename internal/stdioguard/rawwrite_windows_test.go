//go:build windows

package stdioguard

import "golang.org/x/sys/windows"

// rawWriteStdout writes to the process's standard output handle directly, the way a
// native library calling GetStdHandle would, bypassing [os.Stdout].
func rawWriteStdout(b []byte) error {
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return err
	}
	var n uint32
	return windows.WriteFile(h, b, &n, nil)
}

// closeStdout leaves the process with no standard output, as a service or a detached
// process has.
func closeStdout() error {
	return windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, 0)
}

// rawWriteHandle writes to the handle value h, which the caller took from the parent's
// frame writer. In a child that did not inherit it, the value is not that handle.
func rawWriteHandle(h uintptr, b []byte) error {
	var n uint32
	return windows.WriteFile(windows.Handle(h), b, &n, nil)
}
