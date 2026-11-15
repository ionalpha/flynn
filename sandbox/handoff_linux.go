//go:build linux

package sandbox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// socketpair is a variable so a test can drive openHandoff's failure branches,
// which promise that nothing is attached to the command and no descriptor leaks.
var socketpair = unix.Socketpair

// openHandoff wires the launcher's end of a socketpair into c, because a socket
// keeps the namespace it was made in, so only the launcher can create a listener
// the child's connections reach. kind ("egress", "forward") names the handoff;
// envOn and envFD tell the launcher to build it and where to send it.
//
// parent is a net.UnixConn because release may close it while serve blocks on it,
// and closing a raw descriptor mid-read is a use-after-close. child belongs to the
// exec.Cmd and is closed only by release. On error nothing is attached to c.
func openHandoff(c *exec.Cmd, kind, envOn, envFD string) (parent *net.UnixConn, child *os.File, err error) {
	// SOCK_CLOEXEC so an unrelated fork does not inherit it. exec.Cmd clears the flag
	// on the dup it gives the launcher.
	pair, err := socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %s handoff socketpair: %w", kind, err)
	}
	parentFile := os.NewFile(uintptr(pair[0]), "flynn-"+kind+"-handoff")
	child = os.NewFile(uintptr(pair[1]), "flynn-"+kind+"-handoff-child")

	parent, err = unixConnFromFile(parentFile, kind)
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}

	c.ExtraFiles = append(c.ExtraFiles, child)
	childFD := 2 + len(c.ExtraFiles) // ExtraFiles[i] is descriptor 3+i in the child
	c.Env = mergeEnv(c.Env, map[string]string{
		envOn: "1",
		envFD: strconv.Itoa(childFD),
	})
	return parent, child, nil
}

// unixConnFromFile closes f in every case, because FileConn dups it. A descriptor
// that is not a unix socket is an error here, because everything downstream reads
// SCM_RIGHTS off it and would fail less clearly.
func unixConnFromFile(f *os.File, kind string) (*net.UnixConn, error) {
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s handoff conn: %w", kind, err)
	}
	parent, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("sandbox: %s handoff is %T, not a unix socket", kind, conn)
	}
	return parent, nil
}
