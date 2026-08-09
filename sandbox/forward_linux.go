//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/ionalpha/flynn/internal/bindguard"
)

// Control variables passed to the re-executed launcher child for the inbound forward (see
// attachLoopbackForward and serveForwardFromNetns). They tell the launcher to create the
// forward's listening socket inside its network namespace and which inherited descriptor
// to hand it back on. The launcher strips them before running the command.
const (
	envForward   = "FLYNN_SANDBOX_FORWARD"
	envForwardFD = "FLYNN_SANDBOX_FORWARD_FD"
)

// netnsBridgePort is the fixed loopback port the inbound forward listens on inside a
// confined child's network namespace. The namespace is private to the one child and starts
// empty, so a fixed port never collides with anything; using a fixed one means the
// child-facing address is known before the child launches, so it can be baked into the
// command's configuration rather than discovered at run time.
const netnsBridgePort = 8231

// addChild registers the teardown for a forwarder serving one child, under a key its owner
// can drop it by.
func (f *forwardConfig) addChild(key any, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.perChild == nil {
		f.perChild = make(map[any]func())
	}
	f.perChild[key] = release
}

// dropChild forgets one child's teardown, which its own release has already run.
func (f *forwardConfig) dropChild(key any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.perChild, key)
}

// liveChildren counts the per-child forwarders still registered. It exists for the test
// that a finished command does not leave one behind.
func (f *forwardConfig) liveChildren() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.perChild)
}

// ForwardBridge reports how a confined child reaches a bridge listening on the host
// loopback at hostURL: the URL to hand the child, and the host address the sandbox must
// forward it to. On Linux the child runs in its own network namespace and cannot reach the
// host loopback at all, so it is given a fixed in-namespace address and the sandbox
// forwards that to the host one. A hostURL that does not parse is returned unchanged with
// no forward, so a caller that passed something unexpected fails loudly downstream rather
// than silently rewriting it.
func ForwardBridge(hostURL string) (childURL, forwardTo string) {
	u, err := url.Parse(hostURL)
	if err != nil || u.Host == "" {
		return hostURL, ""
	}
	forwardTo = u.Host
	u.Host = fmt.Sprintf("127.0.0.1:%d", netnsBridgePort)
	return u.String(), forwardTo
}

// attachLoopbackForward is the host half of the inbound forward, mirroring attachEgress. It
// hands the launcher one end of a unix socketpair and reads back, as an SCM_RIGHTS
// descriptor, the listening socket the launcher created inside the child's network
// namespace. A socket keeps the namespace it was created in for its whole life, so
// accepting on that descriptor here, in the host namespace, still accepts the connections
// the child makes to its own loopback. Each accepted connection is piped to the one host
// address configured, whose dial runs in this process's host namespace and reaches the
// real bridge. So the child reaches exactly that one host service and nothing else on the
// host loopback.
//
// The returned release ends the forwarder and is called when the child exits; it is also
// registered on the forward config, so a launch that dies before it can release is still
// cleaned up when the sandbox closes.
func (l *Local) attachLoopbackForward(c *exec.Cmd) (func(), error) {
	parent, child, err := openHandoff(c, "forward", envForward, envForwardFD)
	if err != nil {
		return func() {}, err
	}

	h := &forwardHandoff{parent: parent, child: child, hostAddr: l.forward.hostAddr, owner: l.forward}
	l.forward.addChild(h, h.release)
	go h.serve()
	return h.release, nil
}

// forwardHandoff owns one child's inbound forward: the socketpair the launcher hands its
// listening socket back on, the accept loop serving that socket, and the teardown of both.
type forwardHandoff struct {
	parent   *net.UnixConn // this end of the handoff socketpair
	child    *os.File      // the launcher's end; owned by the exec.Cmd, closed only by release
	hostAddr string        // the one host-loopback address connections are piped to
	owner    *forwardConfig

	once sync.Once
	mu   sync.Mutex
	ln   net.Listener
	stop context.CancelFunc
}

// serve waits for the launcher to hand back the socket it created inside the network
// namespace, then accepts on it and pipes each connection to the host address until
// release. A launcher that dies before sending leaves the receive blocked until release,
// and the launch fails on its own error; the child would be in a namespace it cannot reach
// the bridge from, so the failure is closed either way.
func (h *forwardHandoff) serve() {
	fd, err := recvListener(h.parent)
	if err != nil {
		return
	}
	sock := os.NewFile(uintptr(fd), "flynn-forward-listener")
	ln, err := net.FileListener(sock)
	_ = sock.Close()
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.ln, h.stop = ln, cancel
	h.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go h.pipe(ctx, conn)
	}
}

// pipe joins one child connection to a fresh dial of the host address, copying in both
// directions until either side ends or the forwarder is released. The dial runs in the
// host namespace, so it reaches the real bridge; nothing else on the host loopback is
// reachable because nothing else is ever dialled.
func (h *forwardHandoff) pipe(ctx context.Context, child net.Conn) {
	defer func() { _ = child.Close() }()
	// This is not the agent's governed egress: it dials only the one host-loopback address
	// the caller named (the run's own MCP bridge), in the host namespace. netguard denies
	// loopback by design, which is correct for agent egress and wrong here, so the forward's
	// single fixed-target dial is a raw one on purpose.
	var d net.Dialer //nolint:forbidigo // dials only the run's own loopback bridge, not governed agent egress
	host, err := d.DialContext(ctx, "tcp", h.hostAddr)
	if err != nil {
		return
	}
	defer func() { _ = host.Close() }()

	done := make(chan struct{}, 2)
	copyOneWay := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite() // half-close so the peer sees EOF and can finish
		}
		done <- struct{}{}
	}
	go copyOneWay(host, child)
	go copyOneWay(child, host)

	// Return when either direction ends or the forwarder is released; the deferred closes
	// then unblock the other copy, which finishes into the buffered channel without leaking.
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// release tears the forwarder and its accept loop down. It is idempotent, and unblocks a
// serve still waiting on a launcher that never sent.
func (h *forwardHandoff) release() {
	h.once.Do(func() {
		h.owner.dropChild(h)
		h.mu.Lock()
		stop, ln := h.stop, h.ln
		h.mu.Unlock()
		if stop != nil {
			stop()
		}
		if ln != nil {
			_ = ln.Close()
		}
		_ = h.parent.Close()
		_ = h.child.Close()
	})
}

// serveForwardFromNetns is the launcher's half of the inbound forward, run inside the
// child's fresh network namespace after the clone and before the command. It binds the
// fixed forward port on the namespace's loopback and hands that listening socket out to the
// sandbox, which accepts on it from the host namespace and pipes each connection to the one
// host bridge address. A fixed port is safe because this namespace is private to the one
// child and holds nothing else; using a fixed one lets the child-facing address be known
// before launch so it can be baked into the command's configuration.
func serveForwardFromNetns() error {
	handoff, err := forwardHandoffFile()
	if err != nil {
		return err
	}
	defer func() { _ = handoff.Close() }()

	// Idempotent: the egress leg brings loopback up too, and for an external agent both run,
	// but the forward must not depend on the ordering, so it ensures loopback is up itself.
	if err := bringLoopbackUp(); err != nil {
		return err
	}

	ln, err := bindguard.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", netnsBridgePort), bindguard.Loopback())
	if err != nil {
		return fmt.Errorf("forward: listen in network namespace: %w", err)
	}
	// Closed here after its descriptor has been sent: the descriptor in flight over the
	// socketpair holds the socket open, so the sandbox's copy stays valid, and the command
	// does not inherit a listening socket it has no business holding.
	defer func() { _ = ln.Close() }()

	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("forward: listener is %T, not TCP", ln)
	}
	sock, err := tcp.File() // a dup, in blocking mode
	if err != nil {
		return fmt.Errorf("forward: listener descriptor: %w", err)
	}
	defer func() { _ = sock.Close() }()

	if err := unix.Sendmsg(int(handoff.Fd()), []byte{1}, unix.UnixRights(int(sock.Fd())), nil, 0); err != nil {
		return fmt.Errorf("forward: hand the listening socket to the sandbox: %w", err)
	}
	return nil
}

// forwardHandoffFile recovers the inherited forward-handoff descriptor the sandbox passed
// to this launcher, named by number in the environment.
func forwardHandoffFile() (*os.File, error) {
	raw := os.Getenv(envForwardFD)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("forward: malformed handoff descriptor %q", raw)
	}
	return os.NewFile(uintptr(fd), "flynn-forward-handoff"), nil
}
