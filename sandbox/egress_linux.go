//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/ionalpha/flynn/netguard"
)

// The egress launcher variables: build the proxy endpoint in the namespace, and
// the descriptor to return it on. The launcher strips them before the command.
const (
	envEgress   = "FLYNN_SANDBOX_EGRESS"
	envEgressFD = "FLYNN_SANDBOX_EGRESS_FD"
)

// attachEgress receives over SCM_RIGHTS the listener the launcher made in the
// child's routeless namespace, which cannot reach the host loopback. A socket keeps
// its namespace for life, so accepting here takes the child's loopback connections
// while the proxy dials from the host namespace. The missing route enforces it: a
// direct dial fails with ENETUNREACH whatever the child does with the proxy
// variables. One proxy serves one child.
func (l *Local) attachEgress(c *exec.Cmd) (func(), error) {
	parent, child, err := openHandoff(c, "egress", envEgress, envEgressFD)
	if err != nil {
		return func() {}, err
	}

	h := &egressHandoff{parent: parent, child: child, policy: l.egress.policy, owner: l.egress}
	// Registered as well as returned, so a launch that never releases is cleaned up
	// at close.
	l.egress.addChild(h, h.release)
	go h.serve()
	return h.release, nil
}

// addChild registers one child's proxy teardown under key; close runs what remains.
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

// liveChildren counts registered proxies, so a test can see one was not leaked.
func (e *egressConfig) liveChildren() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.perChild)
}

// egressHandoff owns one child's handoff socketpair, proxy and teardown.
type egressHandoff struct {
	parent *net.UnixConn // this end of the handoff socketpair
	// child belongs to the exec.Cmd once in ExtraFiles. Start dups its descriptor,
	// so only release may close it; an earlier close races Start.
	child  *os.File
	policy netguard.Policy
	owner  *egressConfig // where this handoff is registered until it releases

	once sync.Once
	mu   sync.Mutex
	ln   net.Listener
	stop context.CancelFunc
}

// serve runs the proxy on the handed-back listener until release. A launcher that
// dies before sending leaves the receive blocked until release; it cannot get an
// EOF, because the exec.Cmd holds the other end. The child then has no route and
// no endpoint, so the failure is closed.
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

// release is idempotent, and unblocks a serve still waiting on the launcher.
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

// errNoListener means the launcher closed the handoff without sending a listener.
var errNoListener = errors.New("sandbox: egress handoff closed without a listening socket")

// recvListener expects one descriptor and closes any extras. It reads through
// the net.UnixConn so a racing release unblocks it through the runtime poller,
// because closing a descriptor under a blocked syscall is unsafe.
func recvListener(sock *net.UnixConn) (int, error) {
	oob := make([]byte, unix.CmsgSpace(4))
	buf := make([]byte, 1)
	n, oobn, _, _, err := sock.ReadMsgUnix(buf, oob)
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
