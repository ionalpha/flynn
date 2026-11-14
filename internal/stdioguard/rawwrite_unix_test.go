//go:build unix

package stdioguard

import "golang.org/x/sys/unix"

// rawWriteStdout writes to descriptor 1 directly, the way C code, a cgo library or a
// goroutine holding the number 1 would, bypassing [os.Stdout].
func rawWriteStdout(b []byte) error {
	_, err := unix.Write(1, b)
	return err
}

// closeStdout leaves the process with no standard output, as a daemon started with the
// descriptor closed has.
func closeStdout() error { return unix.Close(1) }

// rawWriteHandle writes to the descriptor number h, which the caller took from the
// parent's frame writer. In a child that did not inherit it, the number is unused.
func rawWriteHandle(h uintptr, b []byte) error {
	_, err := unix.Write(int(h), b)
	return err
}
