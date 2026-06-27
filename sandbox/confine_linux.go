//go:build linux

package sandbox

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// Control variables passed to the re-executed launcher child (see reexecConfined
// and RunChildLaunchIfRequested). They carry the working directory and the real
// command across the re-exec; the launcher strips them before running the command.
const (
	envConfine  = "FLYNN_SANDBOX_CONFINE"
	envDir      = "FLYNN_SANDBOX_DIR"
	envArgv     = "FLYNN_SANDBOX_ARGV"
	envReadonly = "FLYNN_SANDBOX_READONLY"
	envSeccomp  = "FLYNN_SANDBOX_SECCOMP"
)

// kernelConfinementSupported reports whether this platform can enforce the network,
// filesystem, and syscall confinement, which it can on Linux.
func kernelConfinementSupported() bool { return true }

// egressEnforceable reports whether governed child egress can be enforced here. The Linux
// leg (a network namespace plus a userspace stack that admits only the proxy) is not built
// yet, so a governed-egress launch refuses rather than running with direct egress open.
func egressEnforceable() bool { return false }

// confine applies the kernel-enforced isolation a Local was configured for to a
// command about to run. With no options it does nothing. Network denial places the
// command in a fresh network namespace (no interfaces, no routes, so no connection
// can be made or accepted). Filesystem confinement places it in a mount namespace
// where the whole host is read-only and only its working directory (plus a private
// scratch area) is writable, so it cannot modify anything outside its working tree.
// Syscall confinement installs a filter that refuses the syscalls a command has no
// honest need for and that would let it escalate privilege or escape. All nest
// inside a user namespace with the caller mapped to root inside it, so an
// unprivileged agent sets up the isolation without real root and the command gains
// no privilege on the host.
func (l *Local) confine(c *exec.Cmd) error {
	if !l.denyNetwork && !l.readonlyFS && !l.seccomp {
		return nil
	}
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	sp := c.SysProcAttr
	sp.Cloneflags |= syscall.CLONE_NEWUSER
	sp.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
	sp.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	// Writing "deny" to setgroups is required before an unprivileged gid mapping is
	// accepted; the Go runtime does this when this flag is false.
	sp.GidMappingsEnableSetgroups = false
	if l.denyNetwork {
		sp.Cloneflags |= syscall.CLONE_NEWNET
	}
	// Filesystem and syscall confinement have to be set up from inside the new
	// process, after the clone and before the command runs, which the standard
	// library does not expose. They go through the re-exec launcher; the mount view
	// additionally needs its own mount namespace.
	if l.readonlyFS || l.seccomp {
		if l.readonlyFS {
			sp.Cloneflags |= syscall.CLONE_NEWNS
		}
		l.reexecConfined(c)
	}
	return nil
}

// reexecConfined rewrites c to run this same binary as a launcher: the binary is
// re-executed inside the new namespaces, applies the filesystem and syscall
// confinement that can only be set up from inside the new process, and only then
// executes the real command. The real command, the working directory, and which
// confinements to apply travel across the re-exec in the environment;
// RunChildLaunchIfRequested picks them up on the other side.
func (l *Local) reexecConfined(c *exec.Cmd) {
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
	}
	if l.seccomp {
		c.Env = append(c.Env, envSeccomp+"=1")
	}
	c.Path = self
	c.Args = []string{self}
}

// RunChildLaunchIfRequested is the other half of filesystem confinement. When this
// binary is re-executed as a confinement launcher (see reexecConfined), this runs
// the mount setup and then replaces the process with the real command, so it never
// returns in that case. In the normal case (not a launcher) it returns immediately
// and the program continues. The program's entry point must call it before doing any
// other work, since a launcher process must not run the normal program at all.
func RunChildLaunchIfRequested() {
	if os.Getenv(envConfine) != "1" {
		return
	}
	os.Exit(runConfinedChild())
}

// runConfinedChild builds the confined filesystem view and execs the real command.
// It returns an exit code only if it fails before the exec replaces the process.
func runConfinedChild() int {
	dir := os.Getenv(envDir)
	argv, err := decodeArgv(os.Getenv(envArgv))
	if err != nil || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: malformed command")
		return 126
	}
	if os.Getenv(envReadonly) == "1" {
		if err := confineMounts(dir); err != nil {
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
	// Syscall filtering is installed last, just before the command runs, so it does
	// not block the launcher's own setup above; it is inherited across the exec. The
	// filter applies to the installing thread, and a thread carries its filter across
	// an exec, so the install and the exec have to happen on the same thread: the lock
	// pins this goroutine to its thread for the rest of its (short) life.
	if os.Getenv(envSeccomp) == "1" {
		runtime.LockOSThread()
		if err := installSeccomp(); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: confinement launcher:", err)
			return 126
		}
	}
	// Exec replaces this process, so on success nothing below runs and the control
	// variables never reach the command (they are stripped from the environment).
	//nolint:gosec // running the sandbox's command is the launcher's purpose; it runs inside the confinement just built (read-only host, isolated mounts, syscall filter), which is the point
	if err := syscall.Exec(bin, argv, strippedEnv()); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox: confinement launcher: exec:", err)
		return 126
	}
	return 0
}

// confineMounts turns the launcher's mount namespace into a read-only view of the
// host with a single writable area: the working directory. It first makes mount
// changes private to this namespace, then remounts every existing mount read-only,
// gives the command a fresh private scratch area so ordinary tooling still works
// (isolated from the host's, and never placed so as to hide the working directory),
// and finally re-grants write access to the working directory alone.
func confineMounts(dir string) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	mps, err := mountPoints()
	if err != nil {
		return err
	}
	for _, mp := range mps {
		// Best effort: some pseudo-filesystems reject a read-only remount, and the
		// host stays read-only regardless because its real mounts are covered.
		_ = syscall.Mount("", mp, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY, "")
	}
	for _, scratch := range []string{"/dev/shm", "/tmp"} {
		if pathWithin(dir, scratch) {
			continue // never shadow the working directory with a scratch mount
		}
		_ = syscall.Mount("tmpfs", scratch, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "")
	}
	if err := syscall.Mount(dir, dir, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind working directory: %w", err)
	}
	if err := syscall.Mount("", dir, "", syscall.MS_REMOUNT|syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("make working directory writable: %w", err)
	}
	return nil
}

// mountPoints reads the current mount points from /proc/self/mountinfo. The mount
// point is the fifth field; paths with spaces or other special characters are octal
// escaped there, so they are decoded back.
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

// unescapeOctal decodes the \NNN octal escapes the kernel uses for special
// characters in mountinfo paths (space is \040, tab \011, newline \012, backslash
// \134).
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

// pathWithin reports whether child is parent or a path nested under it, with both
// already absolute and cleaned.
func pathWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// encodeArgv encodes the command so it can travel across the re-exec in an
// environment variable: each argument is base64-encoded (so it holds no byte that
// the environment cannot, and no comma), and the arguments are joined with commas,
// which the base64 alphabet never contains.
func encodeArgv(args []string) string {
	enc := make([]string, len(args))
	for i, a := range args {
		enc[i] = base64.StdEncoding.EncodeToString([]byte(a))
	}
	return strings.Join(enc, ",")
}

// decodeArgv reverses encodeArgv to recover the command on the other side of the
// re-exec.
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

// strippedEnv returns the process environment with the launcher's control variables
// removed, so the command runs with the clean environment the sandbox built for it.
func strippedEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, envConfine+"="),
			strings.HasPrefix(kv, envDir+"="),
			strings.HasPrefix(kv, envArgv+"="),
			strings.HasPrefix(kv, envReadonly+"="),
			strings.HasPrefix(kv, envSeccomp+"="):
			continue
		}
		out = append(out, kv)
	}
	return out
}
