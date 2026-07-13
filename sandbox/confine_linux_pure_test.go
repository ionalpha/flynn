//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// This file covers the pure decision helpers the Linux confinement launcher relies on. The
// launcher itself runs in a re-executed child inside fresh namespaces (it mounts, remounts,
// and execs), so it cannot run in the test process; these are the functions that decide what
// it does, and each one is a refusal or a boundary check that has to be right for the mount
// namespace it builds to actually confine.

// TestPathWithinBoundsTheWorkingTree proves the containment predicate the launcher uses to
// decide what stays writable accepts only a path at or under the parent, and is not fooled by
// a sibling whose name merely starts with the parent's.
func TestPathWithinBoundsTheWorkingTree(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/work", "/work", true},
		{"/work/sub", "/work", true},
		{"/work/sub/deep", "/work", true},
		{"/work", "/work/sub", false},
		{"/workspace", "/work", false},   // a sibling prefix is not nested
		{"/other", "/work", false},       // unrelated
		{"/", "/work", false},            // the root is not under a subdirectory
		{"/work/sub", "/", true},         // everything is under the filesystem root
		{"/work/../etc", "/work", false}, // a traversal does not land inside
	}
	for _, c := range cases {
		if got := pathWithin(filepath.Clean(c.child), filepath.Clean(c.parent)); got != c.want {
			t.Fatalf("pathWithin(%q, %q) = %v, want %v", c.child, c.parent, got, c.want)
		}
	}
}

// TestShadowsProtectsWritableAreas proves the launcher refuses to lay a fresh scratch tmpfs
// over a directory the child must be able to write. Mounting an empty tmpfs on top of the
// working tree would hide the child's own files, so a scratch mount that would shadow one is
// skipped.
func TestShadowsProtectsWritableAreas(t *testing.T) {
	writable := []string{"/tmp/work", "/var/state"}
	if !shadows("/tmp", writable) {
		t.Fatal("a scratch mount at /tmp would hide the working tree beneath it and must be refused")
	}
	if !shadows("/var/state", writable) {
		t.Fatal("a scratch mount exactly on a writable directory must be refused")
	}
	if shadows("/dev/shm", writable) {
		t.Fatal("a scratch mount that hides nothing writable must be allowed")
	}
	if shadows("/tmp", nil) {
		t.Fatal("with nothing writable, no scratch mount can shadow anything")
	}
}

// TestMountPointsReadsTheHostMounts proves the launcher can enumerate the mounts it must
// remount read-only. Every host has at least a root mount, so an empty result would mean the
// read-only pass silently covered nothing.
func TestMountPointsReadsTheHostMounts(t *testing.T) {
	mps, err := mountPoints()
	if err != nil {
		t.Fatalf("mountPoints: %v", err)
	}
	if len(mps) == 0 {
		t.Fatal("no mount points read; the read-only remount pass would cover nothing")
	}
	found := false
	for _, mp := range mps {
		if mp == "/" {
			found = true
		}
		if !strings.HasPrefix(mp, "/") {
			t.Fatalf("a mount point must be an absolute path, got %q", mp)
		}
	}
	if !found {
		t.Fatal("the root mount must be among the mounts to remount read-only")
	}
}

// TestStrippedEnvRemovesEveryControlVariable proves the launcher's own control variables never
// reach the command it execs: they carry the working directory, the real argv, and the
// confinement switches, and a command that could read (or a later launch that could inherit)
// them would see the sandbox's internals.
func TestStrippedEnvRemovesEveryControlVariable(t *testing.T) {
	controls := []string{
		envConfine, envDir, envArgv, envReadonly, envSeccomp, envWritable,
		envEgress, envEgressFD, envForward, envForwardFD,
	}
	for _, k := range controls {
		t.Setenv(k, "set-by-the-launcher")
	}
	t.Setenv("FLYNN_KEEP_ME", "yes")

	env := strippedEnv()
	for _, kv := range env {
		for _, k := range controls {
			if strings.HasPrefix(kv, k+"=") {
				t.Fatalf("control variable %q survived into the command's environment", k)
			}
		}
	}
	if !slicesContain(env, "FLYNN_KEEP_ME=yes") {
		t.Fatal("a variable the sandbox granted must survive the strip")
	}
}

// TestLinuxConfinementCapabilities proves the platform predicates report what the Linux tier
// actually provides, since a launch path gates on them: a false "cannot" would refuse a
// confinable launch, and a false "can" would run a child unconfined under a tier the trust
// gate relied on.
func TestLinuxConfinementCapabilities(t *testing.T) {
	if !kernelConfinementSupported() {
		t.Fatal("Linux enforces the kernel confinement tier")
	}
	if !egressEnforceable() {
		t.Fatal("Linux enforces governed egress through the child's network namespace")
	}
	if !backgroundConfinementExpressible() {
		t.Fatal("Linux carries the confinement onto a backgrounded process")
	}
	l := newTestLocal(t)
	if got := l.platformConfinementTier(); got != "namespace-seccomp" {
		t.Fatalf("the Linux kernel tier is namespace-seccomp, got %q", got)
	}
}

// TestOpenNoFollowRefusesARedirectedOpen proves the pre-openat2 fallback holds the same
// boundary the kernel-resolved path does: it refuses a terminal symlink outright, and it
// re-validates the opened path so an intermediate symlink that points out of the root cannot
// redirect a read (the time-of-check/time-of-use window an in-tree, model-authored process
// could open).
func TestOpenNoFollowRefusesARedirectedOpen(t *testing.T) {
	l := newTestLocal(t)
	root := l.Root()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "ok.txt")
	if err := os.WriteFile(inside, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A plain file inside the root opens and re-validates.
	f, err := l.openNoFollow(inside, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("a confined file must open: %v", err)
	}
	_ = f.Close()

	// A terminal symlink is refused rather than followed.
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Fatal(err)
	}
	if f, err := l.openNoFollow(link, os.O_RDONLY, 0); !errors.Is(err, ErrDenied) {
		if err == nil {
			_ = f.Close()
		}
		t.Fatalf("a terminal symlink out of the root must be denied, got %v", err)
	}

	// An intermediate symlink is followed by the open, so the post-open re-validation is what
	// denies it: the opened path resolves outside the root.
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	redirected := filepath.Join(root, "out", "secret.txt")
	if f, err := l.openNoFollow(redirected, os.O_RDONLY, 0); !errors.Is(err, ErrDenied) {
		if err == nil {
			_ = f.Close()
		}
		t.Fatalf("an open redirected outside the root by an intermediate symlink must be denied, got %v", err)
	}

	// A path that does not exist surfaces the open error rather than being reported as denied.
	if _, err := l.openNoFollow(filepath.Join(root, "absent.txt"), os.O_RDONLY, 0); err == nil {
		t.Fatal("opening a file that does not exist must be an error")
	}
}

// TestHandoffDescriptorsAreRefusedWhenMalformed proves the launcher will not proceed with a
// handoff descriptor it cannot trust. The egress and forward legs both recover an inherited
// socket named by number in the environment; a missing, unparseable, or reserved (stdio)
// number is refused rather than turned into a file the child would then treat as its
// governed channel, which is what keeps a broken launch from running with egress unenforced.
func TestHandoffDescriptorsAreRefusedWhenMalformed(t *testing.T) {
	for _, bad := range []string{"", "not-a-number", "-1", "0", "2"} {
		t.Setenv(envEgressFD, bad)
		if f, err := egressHandoffFile(); err == nil {
			_ = f.Close()
			t.Fatalf("egress handoff descriptor %q must be refused", bad)
		}
		t.Setenv(envForwardFD, bad)
		if f, err := forwardHandoffFile(); err == nil {
			_ = f.Close()
			t.Fatalf("forward handoff descriptor %q must be refused", bad)
		}
	}
}

// TestHandoffDescriptorsRecoverAnInheritedSocket proves a well-formed descriptor number is
// recovered as the file the sandbox passed, which is how the launcher hands its listening
// socket back out of the child's network namespace.
func TestHandoffDescriptorsRecoverAnInheritedSocket(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	num := strconv.Itoa(fds[0])

	t.Setenv(envEgressFD, num)
	f, err := egressHandoffFile()
	if err != nil {
		t.Fatalf("egress handoff: %v", err)
	}
	if f.Fd() != uintptr(fds[0]) {
		t.Fatalf("the recovered file must be the inherited descriptor, got %d want %d", f.Fd(), fds[0])
	}

	t.Setenv(envForwardFD, num)
	ff, err := forwardHandoffFile()
	if err != nil {
		t.Fatalf("forward handoff: %v", err)
	}
	if ff.Fd() != uintptr(fds[0]) {
		t.Fatalf("the recovered file must be the inherited descriptor, got %d want %d", ff.Fd(), fds[0])
	}
}

// TestLauncherIsInertWithoutItsControlVariable proves the launcher hook is a no-op in a normal
// process: only a process re-executed as a confinement launcher takes over, so a host binary
// calling it at startup is unaffected.
func TestLauncherIsInertWithoutItsControlVariable(t *testing.T) {
	t.Setenv(envConfine, "")
	RunChildLaunchIfRequested() // must return rather than exiting the test process
}

// TestDecodeArgvRoundTripsAndRefusesGarbage proves the command survives the re-exec intact
// (it travels base64-encoded through the environment) and that a corrupted encoding is
// refused rather than exec'ing a mangled command.
func TestDecodeArgvRoundTripsAndRefusesGarbage(t *testing.T) {
	argv := []string{"/bin/sh", "-c", "echo a,b && printf '\n'"}
	got, err := decodeArgv(encodeArgv(argv))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(argv) {
		t.Fatalf("round trip changed the command: %q", got)
	}
	for i := range argv {
		if got[i] != argv[i] {
			t.Fatalf("round trip changed argument %d: %q want %q", i, got[i], argv[i])
		}
	}
	if out, err := decodeArgv(""); err != nil || out != nil {
		t.Fatalf("an empty encoding is no command, got %q err=%v", out, err)
	}
	if _, err := decodeArgv("not-base64!!"); err == nil {
		t.Fatal("a corrupted encoding must be refused, never exec'd")
	}
}
