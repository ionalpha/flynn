//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/ionalpha/flynn/netguard"
)

// Control variables passed to the re-executed launcher child for governed egress (see
// attachEgress and serveEgressFromNetns). They tell the launcher to build the proxy
// endpoint inside its network namespace, and which inherited descriptor to hand the
// listening socket back on. The launcher strips them before running the command.
const (
	envEgress   = "FLYNN_SANDBOX_EGRESS"
	envEgressFD = "FLYNN_SANDBOX_EGRESS_FD"
)

// attachEgress governs a Linux child's outbound network. The child runs in its own
// network namespace, which has no interfaces and no routes, so it cannot reach the
// internet directly and cannot reach a proxy listening on the host's loopback either:
// that loopback is a different interface in a different namespace. The listening socket
// therefore has to be created inside the child's namespace, and only the launcher (which
// runs in there, after the clone and before the command) can create it.
//
// So the launcher creates it and hands it back. This end of the handoff passes the
// launcher one end of a unix socketpair, and reads the listening socket off it as an
// SCM_RIGHTS descriptor. A socket keeps the namespace it was created in for its whole
// life, so accepting on that descriptor here, in the host namespace, still accepts the
// connections the child makes to its own loopback. The proxy then serves those
// connections from this process, where its own outbound dials use the host's namespace
// and reach the network. The one netguard policy decides what they are allowed to reach.
//
// What makes this unbypassable is not the proxy variables in the child's environment,
// which the child is free to ignore. It is that the namespace gives the child no other
// route: a direct dial has no interface to leave through and fails with ENETUNREACH.
// The proxy is not the preferred way out, it is the only one.
//
// One proxy serves one child, because one namespace's loopback is reachable only from
// inside that namespace. The returned release ends that proxy and is called when the
// child exits; it is also registered on the egress config, so a launch that dies before
// it can release is still cleaned up when the sandbox closes.
func (l *Local) attachEgress(c *exec.Cmd) (func(), error) {
	// SOCK_CLOEXEC so an unrelated fork does not inherit the handoff. The descriptor the
	// launcher gets is not this one: exec.Cmd dups ExtraFiles into the child and clears
	// close-on-exec on the dup, which is what lets it survive the launcher's own exec.
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: egress handoff socketpair: %w", err)
	}
	parent := os.NewFile(uintptr(pair[0]), "flynn-egress-handoff")
	child := os.NewFile(uintptr(pair[1]), "flynn-egress-handoff-child")

	c.ExtraFiles = append(c.ExtraFiles, child)
	childFD := 2 + len(c.ExtraFiles) // ExtraFiles[i] is descriptor 3+i in the child
	c.Env = mergeEnv(c.Env, map[string]string{
		envEgress:   "1",
		envEgressFD: strconv.Itoa(childFD),
	})

	h := &egressHandoff{parent: parent, child: child, policy: l.egress.policy, owner: l.egress}
	// Registered on the Local as well as returned: a caller that forgets to release, or a
	// launch that fails before it can, is still cleaned up when the sandbox closes.
	l.egress.addChild(h, h.release)
	go h.serve()
	return h.release, nil
}

// addChild registers the teardown for a proxy serving one child, under a key its owner
// can drop it by. close runs whatever is still registered.
func (e *egressConfig) addChild(key any, release func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.perChild == nil {
		e.perChild = make(map[any]func())
	}
	e.perChild[key] = release
}

// dropChild forgets one child's teardown, which its own release has already run.
func (e *egressConfig) dropChild(key any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.perChild, key)
}

// liveChildren counts the per-child proxies still registered. It exists for the test that
// a finished command does not leave one behind.
func (e *egressConfig) liveChildren() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.perChild)
}

// egressHandoff owns one child's proxy: the socketpair the launcher hands its listening
// socket back on, the proxy serving that socket, and the teardown of both.
type egressHandoff struct {
	parent *os.File // this end of the handoff socketpair
	// child is the launcher's end. It belongs to the exec.Cmd from the moment it is put in
	// ExtraFiles: Start reads its descriptor to dup into the child, so closing it anywhere
	// but release (which runs after the launch is done with it) races that read and can
	// pull the descriptor out from under a launch. serve must not touch it.
	child  *os.File
	policy netguard.Policy
	owner  *egressConfig // where this handoff is registered until it releases

	once sync.Once
	mu   sync.Mutex
	ln   net.Listener
	stop context.CancelFunc
}

// serve waits for the launcher to hand back the socket it created inside the network
// namespace, then serves the egress proxy on it until release. It runs for the life of
// one child.
//
// A launcher that dies before sending (a failed mount setup, a command that does not
// exist) leaves this receive blocked until release, and the launch fails on its own error.
// The receive cannot be given an EOF instead, because the only other holder of the
// launcher's end is the exec.Cmd, which owns it. A handoff that fails here is not a hole:
// the child would be running in a namespace with no route and no endpoint, so it reaches
// nothing. The failure is closed either way.
func (h *egressHandoff) serve() {
	fd, err := recvListener(h.parent)
	if err != nil {
		return
	}
	sock := os.NewFile(uintptr(fd), "flynn-egress-listener")
	ln, err := net.FileListener(sock)
	// FileListener dups the descriptor, so this copy is closed either way.
	_ = sock.Close()
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.ln, h.stop = ln, cancel
	h.mu.Unlock()

	px := netguard.NewProxy(h.policy)
	_ = px.Serve(ctx, ln)
}

// release tears the handoff and its proxy down. It is idempotent, and unblocks a serve
// still waiting on a launcher that never sent.
func (h *egressHandoff) release() {
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

// errNoListener reports a launcher that closed the handoff without sending its listening
// socket, which means the child never got a proxy endpoint.
var errNoListener = errors.New("sandbox: egress handoff closed without a listening socket")

// recvListener blocks until the launcher sends its listening socket over the handoff, and
// returns the received descriptor. Exactly one descriptor is expected; anything else is a
// protocol error rather than something to guess at, and any descriptors received
// alongside are closed rather than leaked.
func recvListener(sock *os.File) (int, error) {
	oob := make([]byte, unix.CmsgSpace(4))
	buf := make([]byte, 1)
	// Fd puts the file into blocking mode, which is what this read wants: it waits for
	// the launcher rather than spinning.
	n, oobn, _, _, err := unix.Recvmsg(int(sock.Fd()), buf, oob, 0)
	if err != nil {
		return -1, fmt.Errorf("sandbox: egress handoff receive: %w", err)
	}
	if n == 0 && oobn == 0 {
		return -1, errNoListener
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("sandbox: egress handoff control message: %w", err)
	}
	if len(scms) != 1 {
		return -1, errNoListener
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, fmt.Errorf("sandbox: egress handoff rights: %w", err)
	}
	if len(fds) == 0 {
		return -1, errNoListener
	}
	for _, extra := range fds[1:] {
		_ = unix.Close(extra)
	}
	unix.CloseOnExec(fds[0])
	return fds[0], nil
}
