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

// rawWriteHandle writes to the handle value h, which the caller took from the parent's
// frame writer. In a child that did not inherit it, the value is not that handle.
func rawWriteHandle(h uintptr, b []byte) error {
	var n uint32
	return windows.WriteFile(windows.Handle(h), b, &n, nil)
}
