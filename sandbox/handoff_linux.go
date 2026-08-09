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

// openHandoff builds the socketpair a launcher hands a listening socket back on, and
// wires the launcher's end into the command about to be run. Both governed-network
// features need it: a socket keeps the network namespace it was created in for its whole
// life, so the only way this process can accept connections made inside the child's
// namespace is for the launcher to create the listener in there and send the descriptor
// out. kind names the handoff in errors and descriptor names ("egress", "forward"); envOn
// and envFD are the control variables telling the launcher to build the endpoint and
// which inherited descriptor to send it back on.
//
// The returned parent is this end of the pair. It is a net.UnixConn rather than a bare
// descriptor because a serve goroutine blocks on it while release may close it (a
// launcher that dies before sending never unblocks the receive), and only a
// poller-managed connection makes that safe: closing a raw descriptor another goroutine
// is blocked on is a use-after-close, and the descriptor number can be reused underneath
// it. child is the launcher's end, owned by the exec.Cmd from here on and closed only by
// the caller's release. On error both ends are closed and nothing has been attached to c.
func openHandoff(c *exec.Cmd, kind, envOn, envFD string) (parent *net.UnixConn, child *os.File, err error) {
	// SOCK_CLOEXEC so an unrelated fork does not inherit the handoff. The descriptor the
	// launcher gets is not this one: exec.Cmd dups ExtraFiles into the child and clears
	// close-on-exec on the dup, which is what lets it survive the launcher's own exec.
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
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

// unixConnFromFile turns one end of the handoff socketpair into the net.UnixConn the
// serve and release goroutines share, and takes ownership of f either way: FileConn dups
// the descriptor, so the original is closed here whether it succeeded or not. kind names
// the handoff in the errors. A descriptor that is not a socket at all, or is a socket of
// some other family, is reported rather than used, because everything downstream reads
// SCM_RIGHTS control messages off it and would otherwise fail somewhere less obvious.
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
