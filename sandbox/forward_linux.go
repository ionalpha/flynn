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

// The inbound forward's launcher variables: create the listener in the namespace,
// and the descriptor to return it on. The launcher strips them before the command.
const (
	envForward   = "FLYNN_SANDBOX_FORWARD"
	envForwardFD = "FLYNN_SANDBOX_FORWARD_FD"
)

// netnsBridgePort is fixed so the child's address is known before launch. The
// namespace is private and empty, so it never collides.
const netnsBridgePort = 8231

// addChild registers one child's forwarder teardown under key.
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

// liveChildren counts registered forwarders, so a test can see one was not leaked.
func (f *forwardConfig) liveChildren() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.perChild)
}

// ForwardBridge returns the URL a confined child uses for a host-loopback bridge
// and the host address to forward it to. The Linux child cannot reach the host
// loopback, so it gets a fixed in-namespace address. An unparseable hostURL is
// returned unchanged with no forward, so the mistake fails downstream.
func ForwardBridge(hostURL string) (childURL, forwardTo string) {
	u, err := url.Parse(hostURL)
	if err != nil || u.Host == "" {
		return hostURL, ""
	}
	forwardTo = u.Host
	u.Host = fmt.Sprintf("127.0.0.1:%d", netnsBridgePort)
	return u.String(), forwardTo
}

// attachLoopbackForward receives over SCM_RIGHTS the listener the launcher made in
// the child's namespace. A socket keeps its namespace for life, so accepting here
// takes the child's loopback connections, and each is piped to the one host
// address. The release is also registered on the forward config, so a launch that
// dies first is cleaned up when the sandbox closes.
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

// forwardHandoff owns one child's handoff socketpair, accept loop and teardown.
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

// serve accepts on the handed-back listener until release. A launcher that dies
// before sending leaves serve blocked until release; the child cannot reach the
// bridge, so the failure is closed.
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

// pipe joins a child connection to a fresh dial of the host address. Nothing else
// on the host loopback is reachable, because nothing else is dialled.
func (h *forwardHandoff) pipe(ctx context.Context, child net.Conn) {
	defer func() { _ = child.Close() }()
	// A raw dial, because netguard denies loopback and this target is the run's
	// own MCP bridge.
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

	// The deferred closes unblock the other copy, which sends into the buffered
	// channel and exits.
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// release is idempotent, and unblocks a serve still waiting on the launcher.
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

// serveForwardFromNetns runs in the child's namespace before the command, binds
// [netnsBridgePort] on its loopback and sends the listener to the sandbox.
func serveForwardFromNetns() error {
	handoff, err := forwardHandoffFile()
	if err != nil {
		return err
	}
	defer func() { _ = handoff.Close() }()

	// The egress leg also brings loopback up; doing it here too removes the
	// ordering dependency.
	if err := bringLoopbackUp(); err != nil {
		return err
	}

	ln, err := bindguard.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", netnsBridgePort), bindguard.Loopback())
	if err != nil {
		return fmt.Errorf("forward: listen in network namespace: %w", err)
	}
	// The sent descriptor keeps the socket open, so closing here only keeps the
	// command from inheriting a listener.
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

// forwardHandoffFile opens the inherited descriptor named in envForwardFD.
func forwardHandoffFile() (*os.File, error) {
	raw := os.Getenv(envForwardFD)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("forward: malformed handoff descriptor %q", raw)
	}
	return os.NewFile(uintptr(fd), "flynn-forward-handoff"), nil
}
