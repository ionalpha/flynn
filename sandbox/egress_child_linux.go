//go:build linux

package sandbox

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/ionalpha/flynn/internal/bindguard"
)

// serveEgressFromNetns runs in the launcher inside the child's new network
// namespace, before the command execs, because only there can the listener be
// made. It raises loopback, listens, and sends the socket to the parent, which
// does all policy and dialling. It returns the proxy variables; a direct dial
// still fails with ENETUNREACH, so containment does not rest on them. Raising
// loopback needs CAP_NET_ADMIN, which confine's user namespace grants.
func serveEgressFromNetns() (map[string]string, error) {
	handoff, err := egressHandoffFile()
	if err != nil {
		return nil, err
	}
	defer func() { _ = handoff.Close() }()

	if err := bringLoopbackUp(); err != nil {
		return nil, err
	}

	// Through bindguard like every listener, so the endpoint binds only loopback.
	ln, err := bindguard.Listen("tcp", "127.0.0.1:0", bindguard.Loopback())
	if err != nil {
		return nil, fmt.Errorf("egress: listen in network namespace: %w", err)
	}
	// The descriptor in flight keeps the socket open for the parent, and closing
	// this copy keeps the command from inheriting a listener.
	defer func() { _ = ln.Close() }()

	addr := ln.Addr().String()
	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("egress: listener is %T, not TCP", ln)
	}
	sock, err := tcp.File() // a dup, in blocking mode
	if err != nil {
		return nil, fmt.Errorf("egress: listener descriptor: %w", err)
	}
	defer func() { _ = sock.Close() }()

	rights := unix.UnixRights(int(sock.Fd()))
	// One payload byte, because a zero-length send carries no control message on
	// some kernels.
	if err := unix.Sendmsg(int(handoff.Fd()), []byte{1}, rights, nil, 0); err != nil {
		return nil, fmt.Errorf("egress: hand the listening socket to the sandbox: %w", err)
	}
	return proxyEnvVars(addr), nil
}

// egressHandoffFile reads the descriptor number from the environment, so it stays
// correct when the launch passes other files.
func egressHandoffFile() (*os.File, error) {
	raw := os.Getenv(envEgressFD)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("egress: malformed handoff descriptor %q", raw)
	}
	return os.NewFile(uintptr(fd), "flynn-egress-handoff"), nil
}

// bringLoopbackUp is needed because a new namespace starts with loopback down, so
// even 127.0.0.1 is unreachable.
func bringLoopbackUp() error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("egress: interface control socket: %w", err)
	}
	defer func() { _ = unix.Close(sock) }()

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("egress: loopback request: %w", err)
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("egress: read loopback flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(sock, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("egress: bring loopback up: %w", err)
	}
	return nil
}
