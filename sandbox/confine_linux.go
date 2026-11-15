//go:build linux

package sandbox

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

// Control variables carry the launch across the re-exec (see reexecConfined);
// the launcher strips them before the command runs.
const (
	envConfine  = "FLYNN_SANDBOX_CONFINE"
	envDir      = "FLYNN_SANDBOX_DIR"
	envArgv     = "FLYNN_SANDBOX_ARGV"
	envReadonly = "FLYNN_SANDBOX_READONLY"
	envSeccomp  = "FLYNN_SANDBOX_SECCOMP"
	envWritable = "FLYNN_SANDBOX_WRITABLE"
	envHidden   = "FLYNN_SANDBOX_UNREADABLE"

	// envPrivateProc asks the launcher to mount its own /proc. The parent decides,
	// because only it sees its mount table before the clone.
	envPrivateProc = "FLYNN_SANDBOX_PRIVATE_PROC"
)

func kernelConfinementSupported() bool { return true }

// egressEnforceable is true because the child's namespace has only loopback and
// no route, so the launcher's proxy endpoint is its only way out.
func egressEnforceable() bool { return true }

// backgroundConfinementExpressible is true because confine works on the exec.Cmd,
// whether it is waited on or left running, so Serve gets Exec's tier.
func backgroundConfinementExpressible() bool { return true }

// confine sets the configured isolation on c. Network denial uses an empty
// network namespace; filesystem confinement a read-only mount namespace with the
// working tree and scratch writable; syscall confinement a filter. All nest in a
// user namespace with the caller mapped to root, so no real root is needed and
// the command gains no host privilege.
func (l *Local) confine(c *exec.Cmd) error {
	if !l.denyNetwork && !l.readonlyFS && !l.seccomp && !l.egressActive() {
		return nil
	}
	// A refusal stops the launch before any child starts; nothing below runs the
	// command with less isolation.
	if !l.selfTest {
		if err := requireConfinement(l.hostNeeds()); err != nil {
			return err
		}
	}
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	sp := c.SysProcAttr
	enterUserNamespace(sp)
	// Egress uses the same empty namespace and reopens one path: a proxy endpoint
	// on its loopback, served under policy.
	if l.denyNetwork || l.egressActive() {
		sp.Cloneflags |= syscall.CLONE_NEWNET
	}
	// These must be set up inside the child between clone and exec, which os/exec
	// does not expose, so a re-exec launcher does it.
	if l.readonlyFS || l.seccomp || l.egressActive() {
		var writable []string
		if l.readonlyFS {
			sp.Cloneflags |= mountNamespaceFlags()
			var err error
			if writable, err = l.grantScratch(c); err != nil {
				return err
			}
		}
		l.reexecConfined(c, writable)
	}
	return nil
}

// enterUserNamespace is shared with the readiness check, so the check tests the
// mapping a launch uses.
func enterUserNamespace(sp *syscall.SysProcAttr) {
	sp.Cloneflags |= syscall.CLONE_NEWUSER
	sp.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
	sp.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	// False makes the runtime write "deny" to setgroups, which an unprivileged gid
	// mapping requires.
	sp.GidMappingsEnableSetgroups = false
}

// reexecConfined rewrites c to re-exec this binary as the launcher, passing the
// command and settings in the environment to RunChildLaunchIfRequested. writable
// is the scratch directory plus the [WithWritableDir] grants.
func (l *Local) reexecConfined(c *exec.Cmd, writable []string) {
	self := "/proc/self/exe"
	if l.selfExe != "" {
		self = l.selfExe
	}
	c.Env = append(
		c.Env,
		envConfine+"=1",
		envDir+"="+l.root,
		envArgv+"="+encodeArgv(c.Args),
	)
	if l.readonlyFS {
		c.Env = append(c.Env, envReadonly+"=1")
		if c.SysProcAttr.Cloneflags&syscall.CLONE_NEWPID != 0 {
			c.Env = append(c.Env, envPrivateProc+"=1")
		}
	}
	if l.seccomp {
		c.Env = append(c.Env, envSeccomp+"=1")
	}
	if len(writable) > 0 {
		c.Env = append(c.Env, envWritable+"="+encodeArgv(writable))
	}
	if len(l.unreadable) > 0 {
		c.Env = append(c.Env, envHidden+"="+encodeArgv(l.unreadable))
	}
	c.Path = self
	c.Args = []string{self}
}

// RunChildLaunchIfRequested must be the first call in main. In a launcher process
// it sets up confinement and execs the command, never returning; otherwise it
// returns at once.
func RunChildLaunchIfRequested() {
	if os.Getenv(envConfine) != "1" {
		return
	}
	os.Exit(runConfinedChild())
}

// runConfinedChild returns only on a failure before exec.
func runConfinedChild() int {
	dir := os.Getenv(envDir)
	argv, err := decodeArgv(os.Getenv(envArgv))
	if err != nil || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: malformed command")
		return 126
	}
	if os.Getenv(envReadonly) == "1" {
		writable, err := decodeArgv(os.Getenv(envWritable))
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: malformed writable directories")
			return 126
		}
		hidden, err := decodeArgv(os.Getenv(envHidden))
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: malformed unreadable paths")
			return 126
		}
		if err := confineMounts(dir, writable, hidden); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: confinement launcher:", err)
			return 126
		}
	}
	// Only a process inside the namespace can create its proxy socket. A failure
	// fails the launch, because the command would otherwise run with no network
	// and no error.
	env := strippedEnv()
	if os.Getenv(envEgress) == "1" {
		proxy, err := serveEgressFromNetns()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: confinement launcher:", err)
			return 126
		}
		env = mergeEnv(env, proxy)
	}
	// The forward socket for the run's MCP bridge is made here for the same reason,
	// and a failure also fails the launch.
	if os.Getenv(envForward) == "1" {
		if err := serveForwardFromNetns(); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: confinement launcher:", err)
			return 126
		}
	}
	if err := syscall.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: chdir:", err)
		return 126
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: command not found:", argv[0])
		return 127
	}
	// The filter goes in last so it cannot block setup. It binds the installing
	// thread and survives exec, so the lock keeps install and exec on one thread.
	if os.Getenv(envSeccomp) == "1" {
		runtime.LockOSThread()
		if err := installSeccomp(); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: confinement launcher:", err)
			return 126
		}
	}
	// On success nothing below runs.
	//nolint:gosec // running the sandbox's command is the launcher's purpose; it runs inside the confinement just built (read-only host, isolated mounts, syscall filter), which is the point
	if err := syscall.Exec(bin, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: exec:", err)
		return 126
	}
	return 0
}

// confineMounts makes mounts private, remounts all read-only, adds a private
// scratch area that never hides the working directory, then re-grants write to
// the working directory and writable dirs. WithUnreadablePaths are hidden last so
// no grant restores one.
//
// Under [envPrivateProc] /proc is mounted first, so the mount table read next is
// the new one. A failure fails the launch; the parent asks only where
// [privateProcAvailable] predicted success.
func confineMounts(dir string, writable, hidden []string) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("%s: %w", msgMakePrivate, err)
	}
	if os.Getenv(envPrivateProc) == "1" {
		// The host's /proc exposes /proc/<pid>/cmdline of every process the caller
		// owns, including a token passed to the runner as a flag.
		if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC|keptMountFlags("/proc"), ""); err != nil {
			return fmt.Errorf("mount a private /proc: %w", err)
		}
	}
	mps, err := mountPoints()
	if err != nil {
		return err
	}
	for _, mp := range mps {
		// Some pseudo-filesystems reject this; the real mounts are covered anyway.
		_ = syscall.Mount("", mp, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|keptMountFlags(mp), "")
	}
	for _, scratch := range []string{"/dev/shm", "/tmp"} {
		if shadows(scratch, append([]string{dir}, writable...)) {
			continue // never shadow a writable area with a scratch mount
		}
		_ = syscall.Mount("tmpfs", scratch, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "")
	}
	if err := remountWritable(dir); err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	// After the working directory, so a nested one cannot undo that grant. A failure
	// fails the launch so the child never finds the grant missing mid-run.
	for _, w := range writable {
		if pathWithin(w, dir) {
			continue // already writable as part of the working directory
		}
		if err := remountWritable(w); err != nil {
			return fmt.Errorf("writable directory %s: %w", w, err)
		}
	}
	return hideUnreadable(hidden)
}

// mountNamespaceFlags adds a PID namespace where [privateProcAvailable] allows a
// private /proc. [Local.confine] and the readiness check share it, so a unit that
// forbids PID namespaces fails the check instead of every launch.
func mountNamespaceFlags() uintptr {
	if privateProcAvailable() {
		return syscall.CLONE_NEWNS | syscall.CLONE_NEWPID
	}
	return syscall.CLONE_NEWNS
}

// privateProcAvailable is false when anything is mounted below /proc. Container
// runtimes mask /proc/kcore, /proc/acpi and similar, and the kernel refuses
// (EPERM) an unprivileged procfs that would reveal them. There the command keeps
// the host /proc and can read the runner's cmdline, so keep secrets off it.
func privateProcAvailable() bool {
	mps, err := mountPoints()
	if err != nil {
		return false
	}
	return !procMasked(mps)
}

// procMasked ignores /proc/sys/fs/binfmt_misc: systemd mounts it everywhere and
// the kernel treats it as permanently empty, so it hides nothing.
func procMasked(mps []string) bool {
	for _, mp := range mps {
		if mp != "/proc/sys/fs/binfmt_misc" && strings.HasPrefix(mp, "/proc/") {
			return true
		}
	}
	return false
}

// remountWritable bind-mounts dir onto itself to give it its own mount, then
// remounts that writable.
func remountWritable(dir string) error {
	if err := syscall.Mount(dir, dir, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	if err := syscall.Mount("", dir, "", syscall.MS_REMOUNT|syscall.MS_BIND|keptMountFlags(dir), ""); err != nil {
		return fmt.Errorf("make writable: %w", err)
	}
	return nil
}

// restrictiveMountFlags only remove capabilities. The kernel refuses a remount
// that clears one on a mount from outside the user namespace, where it is locked.
const restrictiveMountFlags = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_NOATIME | syscall.MS_NODIRATIME

// keptMountFlags returns path's restrictive flags so a remount changes only the
// read-only bit. Under NoNewPrivileges=true, / and /tmp are nosuid, and a remount
// dropping it fails with EPERM, refusing every launch and leaving the read-only
// remount undone. An unreadable path yields none.
func keptMountFlags(path string) uintptr {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return keepRestrictive(uint64(st.Flags)) //nolint:gosec // G115: statfs flags are small non-negative mount bits
}

// keepRestrictive relies on statfs using the mount(2) values for these bits.
func keepRestrictive(statfsFlags uint64) uintptr {
	return uintptr(statfsFlags & restrictiveMountFlags)
}

// shadows reports whether mp is any of dirs or an ancestor of one.
func shadows(mp string, dirs []string) bool {
	for _, d := range dirs {
		if pathWithin(d, mp) {
			return true
		}
	}
	return false
}

// mountPoints reads field five of /proc/self/mountinfo, decoding its octal
// escapes.
func mountPoints() ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read mounts: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		out = append(out, unescapeOctal(fields[4]))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan mounts: %w", err)
	}
	return out, nil
}

// unescapeOctal decodes mountinfo's \NNN escapes (space \040, tab \011, newline
// \012, backslash \134).
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			ok := true
			for j := 1; j <= 3; j++ {
				d := s[i+j]
				if d < '0' || d > '7' {
					ok = false
					break
				}
				v = v*8 + int(d-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// encodeArgv base64-encodes each argument and joins them with commas, which
// base64 never contains, so argv fits in one environment variable.
func encodeArgv(args []string) string {
	enc := make([]string, len(args))
	for i, a := range args {
		enc[i] = base64.StdEncoding.EncodeToString([]byte(a))
	}
	return strings.Join(enc, ",")
}

func decodeArgv(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, len(parts))
	for i, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil, fmt.Errorf("decode argv: %w", err)
		}
		out[i] = string(raw)
	}
	return out, nil
}

// strippedEnv removes the control variables.
func strippedEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, envConfine+"="),
			strings.HasPrefix(kv, envDir+"="),
			strings.HasPrefix(kv, envArgv+"="),
			strings.HasPrefix(kv, envReadonly+"="),
			strings.HasPrefix(kv, envWritable+"="),
			strings.HasPrefix(kv, envHidden+"="),
			strings.HasPrefix(kv, envPrivateProc+"="),
			strings.HasPrefix(kv, envEgress+"="),
			strings.HasPrefix(kv, envEgressFD+"="),
			strings.HasPrefix(kv, envForward+"="),
			strings.HasPrefix(kv, envForwardFD+"="),
			strings.HasPrefix(kv, envSeccomp+"="):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// platformConfinementTier ignores WithHostReadable, because a read-only mount
// namespace already permits reads.
func (l *Local) platformConfinementTier() string { return tierNamespaceSeccomp }
