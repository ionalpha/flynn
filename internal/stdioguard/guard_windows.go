//go:build windows

package stdioguard

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// redirectStdout keeps the original stdout as a private handle and makes the process's
// standard output handle the write end of a pipe.
//
// Windows has no descriptor table to rewrite. Code reaches stdout through the standard
// handle, which [windows.SetStdHandle] replaces, and through [os.Stdout], a Go variable
// that captured the old handle at startup and is replaced as well.
//
// The private handle is a duplicate that is not inheritable, and the original is marked
// not inheritable too. A child normally gets only the handles named for it, because Go's
// process start and the sandbox's AppContainer launch both pass an explicit handle list;
// clearing inheritance covers any launch that inherits every inheritable handle instead.
// The original is not closed because standard output and standard error can be the same
// handle, and closing it would take standard error with it.
func redirectStdout() (frames, strays *os.File, err error) {
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNoStdout, err)
	}
	if h == 0 || h == windows.InvalidHandle {
		return nil, nil, ErrNoStdout
	}
	self := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(self, h, self, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, fmt.Errorf("stdioguard: duplicate stdout handle: %w", err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = windows.CloseHandle(dup)
		return nil, nil, fmt.Errorf("stdioguard: create stray pipe: %w", err)
	}
	fail := func(what string, err error) (*os.File, *os.File, error) {
		_ = windows.CloseHandle(dup)
		_ = pr.Close()
		_ = pw.Close()
		return nil, nil, fmt.Errorf("stdioguard: %s: %w", what, err)
	}
	// The read end must stay ours: a child holding it could steal the strays.
	if err := windows.SetHandleInformation(windows.Handle(pr.Fd()), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fail("restrict stray pipe", err)
	}
	if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fail("restrict original stdout", err)
	}
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(pw.Fd())); err != nil {
		return fail("repoint stdout handle", err)
	}
	// os.Stdout is the only reference to the pipe's write end; a collected file would close
	// the handle the process now treats as standard output.
	os.Stdout = pw
	return os.NewFile(uintptr(dup), "stdio-frames"), pr, nil
}
