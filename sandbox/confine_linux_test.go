//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestNetworkDeniedHasNoRoutes proves the kernel enforcement: a command run under
// WithNetworkDenied lands in a network namespace with no routes, so it cannot reach
// anything, while the same command without the option sees the host's routes. The
// test skips where unprivileged user namespaces are unavailable (some locked-down
// hosts), so it verifies the isolation where the kernel allows it without failing
// where it cannot be set up at all.
func TestNetworkDeniedHasNoRoutes(t *testing.T) {
	ctx := context.Background()

	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	res, err := denied.Exec(ctx, Command{Line: "cat /proc/net/route"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/network namespaces unavailable on this host")
		}
		t.Fatalf("denied exec: %v", err)
	}
	if n := routeEntries(res.Output); n != 0 {
		t.Fatalf("a network-denied command must see no routes, saw %d:\n%s", n, res.Output)
	}

	// Sanity: a normal command on the same host does have routes, so the test is
	// actually observing the isolation and not a host that simply has none.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err = open.Exec(ctx, Command{Line: "cat /proc/net/route"})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	if routeEntries(res.Output) == 0 {
		t.Skip("host itself has no routes; cannot distinguish isolation here")
	}
}

// TestNetworkDeniedBlocksConnect confirms a command cannot open an outbound
// connection when the network is denied.
func TestNetworkDeniedBlocksConnect(t *testing.T) {
	denied, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	// A non-loopback connect must fail with no route; a non-zero exit is the pass.
	res, err := denied.Exec(context.Background(), Command{Line: "timeout 3 bash -c 'exec 3<>/dev/tcp/8.8.8.8/53' 2>&1"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/network namespaces unavailable on this host")
		}
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an outbound connect must fail under network deny, but it succeeded:\n%s", res.Output)
	}
}

// TestReadOnlyFSWritesOnlyWorkdir proves the filesystem confinement: a command run
// under WithReadOnlyFS can write its own working directory but cannot write anywhere
// else on the host, even to a directory the running user owns (which a plain user
// namespace would leave writable). It also confirms the host stays readable, so the
// confinement restricts writes without blinding the command.
func TestReadOnlyFSWritesOnlyWorkdir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir() // a sibling the test user owns, outside the sandbox root

	sb, err := NewLocal(root, WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}

	// A write inside the working directory succeeds and lands on disk.
	res, err := sb.Exec(ctx, Command{Line: "echo inside > made.txt"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/mount namespaces unavailable on this host")
		}
		t.Fatalf("workdir write exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("a write to the working directory must succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "made.txt")); err != nil {
		t.Fatalf("the working-directory write did not land: %v", err)
	}

	// A write to a user-owned directory outside the working tree is refused: the
	// host is read-only, so this is the gap a plain user namespace would leave open.
	res, err = sb.Exec(ctx, Command{Line: "echo escape > " + filepath.Join(outside, "escape.txt")})
	if err != nil {
		t.Fatalf("outside write exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a write outside the working tree must fail under a read-only host, but it succeeded:\n%s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); err == nil {
		t.Fatal("a file was written outside the working tree under a read-only host")
	}

	// Reads still work: confinement restricts writes, it does not blind the command.
	res, err = sb.Exec(ctx, Command{Line: "cat /proc/self/status > /dev/null"})
	if err != nil {
		t.Fatalf("read exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("a read of a host file must still succeed, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// TestReadOnlyFSWithNetworkDenied confirms the two kernel confinements compose: a
// command run under both sees no routes and cannot write outside its working tree.
func TestReadOnlyFSWithNetworkDenied(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()

	sb, err := NewLocal(root, WithReadOnlyFS(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}

	res, err := sb.Exec(ctx, Command{Line: "cat /proc/net/route"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user/mount/network namespaces unavailable on this host")
		}
		t.Fatalf("exec: %v", err)
	}
	if n := routeEntries(res.Output); n != 0 {
		t.Fatalf("a network-denied command must see no routes, saw %d:\n%s", n, res.Output)
	}

	res, err = sb.Exec(ctx, Command{Line: "echo escape > " + filepath.Join(outside, "escape.txt")})
	if err != nil {
		t.Fatalf("outside write exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a write outside the working tree must fail when the host is read-only:\n%s", res.Output)
	}
}

// TestDefaultConfinementConfines proves the secure-by-default baseline actually
// confines on a platform that enforces it: without the caller naming any option, a
// command cannot write outside its working tree and cannot make a denied syscall, the
// same protection the explicit options give.
func TestDefaultConfinementConfines(t *testing.T) {
	ctx := context.Background()
	// The default baseline falls back to the floor where the kernel will not enforce
	// the confinement, so there is nothing to assert there; skip unless it is
	// enforceable on this host.
	if !confinementEnforceable(t) {
		t.Skip("kernel confinement is not enforceable on this host; the default falls back to the floor")
	}
	root := t.TempDir()
	outside := t.TempDir()

	sb, err := NewLocal(root, WithDefaultConfinement())
	if err != nil {
		t.Fatal(err)
	}

	res, err := sb.Exec(ctx, Command{Line: "echo x > " + filepath.Join(outside, "escape.txt")})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("the default confinement must block a write outside the working tree:\n%s", res.Output)
	}

	res, err = sb.Exec(ctx, Command{Line: "unshare --user --map-root-user true"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Output, "not found") && res.ExitCode == 0 {
		t.Fatalf("the default confinement must filter a dangerous syscall:\n%s", res.Output)
	}
}

// TestSeccompBlocksDangerousSyscall proves the syscall filter: a command run under
// WithSeccomp cannot make a denied syscall (here unshare, which creates new
// namespaces), while the same command without the filter can. A refused call fails
// rather than killing the command, so a non-zero exit is the pass. The test is a
// differential so it observes the filter and not a host that forbids the call anyway.
func TestSeccompBlocksDangerousSyscall(t *testing.T) {
	ctx := context.Background()

	filtered, err := NewLocal(t.TempDir(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	res, err := filtered.Exec(ctx, Command{Line: "unshare --user --map-root-user true"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user namespaces unavailable on this host")
		}
		t.Fatalf("filtered exec: %v", err)
	}
	if strings.Contains(res.Output, "not found") {
		t.Skip("unshare command not available on this host")
	}
	if res.ExitCode == 0 {
		t.Fatalf("a denied syscall must fail under the filter, but unshare succeeded:\n%s", res.Output)
	}

	// Sanity: without the filter the same command succeeds, so the test is observing
	// the filter and not a host that simply forbids unshare.
	open, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err = open.Exec(ctx, Command{Line: "unshare --user --map-root-user true"})
	if err != nil {
		t.Fatalf("open exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("host forbids unshare without the filter, cannot distinguish here:\n%s", res.Output)
	}
}

// TestSeccompAllowsOrdinaryCommand confirms the filter does not break normal work: a
// command using only ordinary syscalls runs and produces its output unaffected.
func TestSeccompAllowsOrdinaryCommand(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithSeccomp())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: "echo confined && cat /proc/self/status > /dev/null"})
	if err != nil {
		if namespaceUnavailable(err.Error()) {
			t.Skip("unprivileged user namespaces unavailable on this host")
		}
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "confined") {
		t.Fatalf("an ordinary command must run unaffected under the filter, got exit %d:\n%s", res.ExitCode, res.Output)
	}
}

// routeEntries counts route table rows in /proc/net/route, excluding the header, so
// zero means the namespace has no routes at all.
func routeEntries(procNetRoute string) int {
	lines := strings.Split(strings.TrimRight(procNetRoute, "\n"), "\n")
	if len(lines) <= 1 {
		return 0
	}
	return len(lines) - 1
}

// namespaceUnavailable reports whether an exec error is a namespace-setup failure
// (the host forbids unprivileged user namespaces) rather than a real problem.
func namespaceUnavailable(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "invalid argument") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "clone") ||
		strings.Contains(msg, "confinement refused")
}

// confinementEnforceable reports whether this host actually sets up the kernel
// confinement. It makes an explicit request, which fails loudly when the kernel will
// not provide it (for example where unprivileged user namespaces are restricted), and
// reports whether the command ran. Tests of the always-on default use it to skip
// where the default would fall back to the floor.
func confinementEnforceable(t *testing.T) bool {
	t.Helper()
	sb, err := NewLocal(t.TempDir(), WithReadOnlyFS())
	if err != nil {
		t.Fatal(err)
	}
	_, err = sb.Exec(context.Background(), Command{Line: "true"})
	return err == nil
}

// TestKeepRestrictiveSelectsOnlyFlagsThatTakeAwayCapability holds the mask to the flags
// that restrict a mount: keeping a flag across a remount can never grant anything, so the
// mask must not pass the read-only flag (the one the launcher changes on purpose) or any
// flag that loosens a mount.
func TestKeepRestrictiveSelectsOnlyFlagsThatTakeAwayCapability(t *testing.T) {
	tests := []struct {
		name  string
		flags uint64
		want  uintptr
	}{
		{"none", 0, 0},
		{"nosuid", syscall.MS_NOSUID, syscall.MS_NOSUID},
		{"nosuid nodev noexec", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC},
		{"noatime nodiratime", syscall.MS_NOATIME | syscall.MS_NODIRATIME, syscall.MS_NOATIME | syscall.MS_NODIRATIME},
		{"read-only is not kept", syscall.MS_RDONLY | syscall.MS_NOSUID, syscall.MS_NOSUID},
		{"relatime and sync are not kept", syscall.MS_RELATIME | syscall.MS_SYNCHRONOUS | syscall.MS_MANDLOCK, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepRestrictive(tc.flags); got != tc.want {
				t.Fatalf("keepRestrictive(%#x) = %#x, want %#x", tc.flags, got, tc.want)
			}
		})
	}
}

// envLockedMountDir marks the helper process of
// TestReadOnlyFSSucceedsOnAMountWithLockedFlags and names the directory it works in.
const envLockedMountDir = "FLYNN_TEST_LOCKED_MOUNT_DIR"

// exitHelperUnavailable is the helper's exit code when it cannot build the situation the
// test needs (its own namespaces or mount), as against the launch under test failing.
const exitHelperUnavailable = 77

// TestReadOnlyFSSucceedsOnAMountWithLockedFlags reproduces what a service unit with
// NoNewPrivileges=true does to the launcher: the working directory sits on a nosuid mount
// that was made outside the launcher's user namespace, so the kernel locks the flag. A
// remount that clears it fails with EPERM, and the launcher refused every launch. The
// helper process stands in for the service: it mounts a nosuid tmpfs in its own user and
// mount namespaces, then runs a confined command whose working directory is on it.
func TestReadOnlyFSSucceedsOnAMountWithLockedFlags(t *testing.T) {
	if dir := os.Getenv(envLockedMountDir); dir != "" {
		os.Exit(lockedMountHelper(dir))
	}
	//nolint:gosec // re-executing the test binary is the helper-process pattern
	c := exec.Command(os.Args[0], "-test.run=^TestReadOnlyFSSucceedsOnAMountWithLockedFlags$")
	c.Env = append(os.Environ(), envLockedMountDir+"="+t.TempDir())
	c.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case !errors.As(err, &exit):
		t.Skipf("cannot start a process in new user and mount namespaces here: %v", err)
	case exit.ExitCode() == exitHelperUnavailable:
		t.Skipf("cannot build a nosuid mount in a user namespace here:\n%s", out)
	default:
		t.Fatalf("a confined command on a mount with locked flags failed: %v\n%s", err, out)
	}
}

// lockedMountHelper is the body of the helper process. It returns the process exit code.
func lockedMountHelper(dir string) int {
	fail := func(code int, format string, args ...any) int {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		return code
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fail(exitHelperUnavailable, "make mounts private: %v", err)
	}
	if err := syscall.Mount("tmpfs", dir, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fail(exitHelperUnavailable, "mount nosuid tmpfs: %v", err)
	}
	l, err := NewLocal(dir, withoutGate(), WithReadOnlyFS())
	if err != nil {
		return fail(1, "new local: %v", err)
	}
	res, err := l.Exec(context.Background(), Command{Line: "echo ok > made.txt"})
	if err != nil {
		return fail(1, "exec: %v", err)
	}
	if res.ExitCode != 0 {
		return fail(1, "exit %d:\n%s", res.ExitCode, res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "made.txt")); err != nil {
		return fail(1, "the write did not land: %v", err)
	}
	return 0
}
