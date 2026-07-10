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

// serveEgressFromNetns is the launcher's half of governed egress, and it runs in the one
// place that can do this work: inside the child's fresh network namespace, after the
// clone and before the command is executed.
//
// It brings the namespace's loopback interface up (a fresh namespace has it down, so
// nothing can be reached on it, not even 127.0.0.1), creates the listening socket the
// proxy will serve, and hands that socket out to the parent over the inherited handoff
// descriptor. The parent accepts on it from the host namespace and does the policy
// enforcement and the real dialling, so no networking code beyond a bare listen runs in
// here, and nothing outside the namespace is reachable from in here.
//
// It returns the proxy variables to add to the command's environment. The command has no
// other route out: its namespace has no interface but loopback and no route at all, so a
// direct dial fails with ENETUNREACH. Pointing it at the proxy is a convenience for a
// command that reads these variables; the containment does not depend on it doing so.
//
// Bringing loopback up needs CAP_NET_ADMIN in the namespace's owning user namespace, and
// the launcher has it: confine maps the caller to root in a new user namespace, and the
// network namespace is created inside it. No host privilege is involved.
func serveEgressFromNetns() (map[string]string, error) {
	handoff, err := egressHandoffFile()
	if err != nil {
		return nil, err
	}
	defer func() { _ = handoff.Close() }()

	if err := bringLoopbackUp(); err != nil {
		return nil, err
	}

	// Through bindguard like every other listener, so the endpoint cannot be opened on
	// anything but loopback. Here that loopback is the namespace's own, reachable only by
	// this child, but the bind still goes through the one governed path.
	ln, err := bindguard.Listen("tcp", "127.0.0.1:0", bindguard.Loopback())
	if err != nil {
		return nil, fmt.Errorf("egress: listen in network namespace: %w", err)
	}
	// The listener is closed here after its descriptor has been sent: the descriptor in
	// flight over the socketpair holds the socket open, so the parent's copy stays valid.
	// Closing this copy keeps the command from inheriting a listening socket it has no
	// business holding.
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
	// One payload byte alongside the descriptor: a zero-length send carries no control
	// message on some kernels, and the parent distinguishes "sent nothing" from "sent a
	// socket" by what arrives.
	if err := unix.Sendmsg(int(handoff.Fd()), []byte{1}, rights, nil, 0); err != nil {
		return nil, fmt.Errorf("egress: hand the listening socket to the sandbox: %w", err)
	}
	return proxyEnvVars(addr), nil
}

// egressHandoffFile recovers the inherited handoff descriptor the sandbox passed to this
// launcher. It is named by number in the environment rather than assumed to be a fixed
// descriptor, so it stays correct if the launch ever passes other files.
func egressHandoffFile() (*os.File, error) {
	raw := os.Getenv(envEgressFD)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("egress: malformed handoff descriptor %q", raw)
	}
	return os.NewFile(uintptr(fd), "flynn-egress-handoff"), nil
}

// bringLoopbackUp raises the loopback interface of the network namespace this process is
// in. A fresh namespace starts with loopback administratively down, so without this even
// 127.0.0.1 is unreachable and the child could not reach the proxy endpoint.
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
