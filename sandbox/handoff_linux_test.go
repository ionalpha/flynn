//go:build linux

package sandbox

import (
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The handoff is what lets a listening socket created inside the child's network
// namespace be accepted on from the host, so both governed egress and the inbound
// forward are built on it. It needs no namespace of its own, so unlike the probes that
// exercise it end to end, these run on any Linux host.

// The descriptor number handed to the launcher has to match where exec.Cmd actually puts
// the file, and nothing at run time would catch a mismatch: the launcher would read some
// unrelated inherited descriptor and hang waiting for a socket that never arrives. So
// the arithmetic is pinned here, with a second handoff attached to prove it counts the
// files already on the command rather than assuming it is the first.
func TestOpenHandoffTellsTheLauncherWhereItsEndLands(t *testing.T) {
	c := exec.Command("true")

	parent, child, err := openHandoff(c, "egress", envEgress, envEgressFD)
	if err != nil {
		t.Fatalf("open the first handoff: %v", err)
	}
	t.Cleanup(func() { _ = parent.Close(); _ = child.Close() })

	if got := len(c.ExtraFiles); got != 1 {
		t.Fatalf("expected the launcher's end to be attached to the command, got %d extra files", got)
	}
	if c.ExtraFiles[0] != child {
		t.Fatal("expected the attached file to be the launcher's end of the pair")
	}
	assertEnv(t, c.Env, envEgress, "1")
	assertEnv(t, c.Env, envEgressFD, "3")

	parent2, child2, err := openHandoff(c, "forward", envForward, envForwardFD)
	if err != nil {
		t.Fatalf("open the second handoff: %v", err)
	}
	t.Cleanup(func() { _ = parent2.Close(); _ = child2.Close() })

	assertEnv(t, c.Env, envForward, "1")
	assertEnv(t, c.Env, envForwardFD, "4")
	// The first handoff's variables survive the second: a child gets both.
	assertEnv(t, c.Env, envEgressFD, "3")
}

// The two ends are one socketpair, so what the launcher writes arrives here. This is the
// property the SCM_RIGHTS receive depends on; a pair that was not actually connected
// would leave every governed launch blocked on a receive instead of failing.
func TestOpenHandoffConnectsTheTwoEnds(t *testing.T) {
	c := exec.Command("true")
	parent, child, err := openHandoff(c, "egress", envEgress, envEgressFD)
	if err != nil {
		t.Fatalf("open the handoff: %v", err)
	}
	t.Cleanup(func() { _ = parent.Close(); _ = child.Close() })

	if _, err := child.Write([]byte("x")); err != nil {
		t.Fatalf("write from the launcher's end: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := parent.Read(buf); err != nil {
		t.Fatalf("read on this end: %v", err)
	}
	if buf[0] != 'x' {
		t.Fatalf("expected the byte the launcher's end sent, got %q", buf[0])
	}
}

// A descriptor that is not a socket cannot carry the SCM_RIGHTS message the launcher
// sends, so it is refused at the handoff rather than accepted and failed later inside a
// serve goroutine, where the error would have nowhere to go.
func TestUnixConnFromFileRefusesSomethingThatIsNotASocket(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-socket")
	if err != nil {
		t.Fatalf("create the file: %v", err)
	}

	conn, err := unixConnFromFile(f, "egress")
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected a regular file to be refused as a handoff")
	}
	if !strings.Contains(err.Error(), "egress handoff conn") {
		t.Fatalf("expected the error to name the egress handoff, got %v", err)
	}
}

// A socket of the wrong family is the subtler case: FileConn accepts it and returns a
// working connection, just not one that can carry descriptors. Refusing it by type keeps
// the failure at the handoff.
func TestUnixConnFromFileRefusesASocketOfAnotherFamily(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	tcp, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = tcp.Close() }()
	f, err := tcp.(*net.TCPConn).File()
	if err != nil {
		t.Fatalf("take the connection's file: %v", err)
	}

	conn, err := unixConnFromFile(f, "forward")
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected a TCP socket to be refused as a handoff")
	}
	if !strings.Contains(err.Error(), "not a unix socket") {
		t.Fatalf("expected the error to say the socket is the wrong kind, got %v", err)
	}
}

// Out of descriptors is the one way the kernel refuses a socketpair. The caller then goes
// on to build the command without governed networking, so the failure has to leave the
// command exactly as it found it; an ExtraFiles entry or a control variable left behind
// would tell the launcher to wait for a handoff that will never come.
func TestOpenHandoffLeavesTheCommandAloneWhenTheSocketpairFails(t *testing.T) {
	withSocketpair(t, func(int, int, int) ([2]int, error) { return [2]int{}, unix.EMFILE })

	c := exec.Command("true")
	c.Env = []string{"PATH=/usr/bin"}

	parent, child, err := openHandoff(c, "egress", envEgress, envEgressFD)
	if err == nil {
		_ = parent.Close()
		_ = child.Close()
		t.Fatal("expected a refused socketpair to fail the handoff")
	}
	if !strings.Contains(err.Error(), "egress handoff socketpair") {
		t.Fatalf("expected the error to name the egress socketpair, got %v", err)
	}
	if len(c.ExtraFiles) != 0 {
		t.Fatalf("expected no file attached to the command, got %d", len(c.ExtraFiles))
	}
	if !slices.Equal(c.Env, []string{"PATH=/usr/bin"}) {
		t.Fatalf("expected the environment untouched, got %v", c.Env)
	}
}

// The other branch: a pair that is not a unix socket. The launcher's end has already been
// wrapped in an *os.File by this point and is owned by nothing yet, so the handoff has to
// close it itself or the descriptor leaks on every failed launch.
func TestOpenHandoffClosesTheLaunchersEndWhenTheConnectionCannotBeBuilt(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-socket")
	if err != nil {
		t.Fatalf("create the file: %v", err)
	}
	defer func() { _ = f.Close() }()
	first, err := unix.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup the file: %v", err)
	}
	second, err := unix.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup the file: %v", err)
	}
	withSocketpair(t, func(int, int, int) ([2]int, error) { return [2]int{first, second}, nil })

	c := exec.Command("true")
	parent, child, err := openHandoff(c, "forward", envForward, envForwardFD)
	if err == nil {
		_ = parent.Close()
		_ = child.Close()
		t.Fatal("expected a pair that is not a unix socket to fail the handoff")
	}
	if !strings.Contains(err.Error(), "forward handoff conn") {
		t.Fatalf("expected the error to name the forward handoff, got %v", err)
	}
	if len(c.ExtraFiles) != 0 {
		t.Fatalf("expected no file attached to the command, got %d", len(c.ExtraFiles))
	}
	// Both ends are closed on the way out, so neither descriptor is still open.
	if err := unix.Close(second); err != unix.EBADF {
		t.Fatalf("expected the launcher's end to have been closed, got %v", err)
	}
}

func withSocketpair(t *testing.T, stub func(int, int, int) ([2]int, error)) {
	t.Helper()
	prev := socketpair
	socketpair = stub
	t.Cleanup(func() { socketpair = prev })
}

func assertEnv(t *testing.T, env []string, key, want string) {
	t.Helper()
	entry := key + "=" + want
	if !slices.Contains(env, entry) {
		t.Fatalf("expected %q in the child's environment, got %v", entry, env)
	}
}
