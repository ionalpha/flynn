package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/internal/closeerr"
	"github.com/ionalpha/flynn/internal/procexec"
)

// Local is the in-process tier and runs on every host. File operations are
// confined to a root: "..", an absolute path or an escaping symlink is denied.
// A command it runs otherwise has the host user's privileges, so it is not
// strong isolation alone. WithNetworkDenied, WithReadOnlyFS and WithSeccomp add
// kernel isolation where the platform supports it. Local is the FS floor under
// every other tier.
type Local struct {
	root        string // absolute, symlinks resolved
	execTimeout time.Duration
	resLimits   ResourceLimits    // host memory/process caps applied where the platform can (see WithResourceLimits)
	granted     map[string]string // env vars explicitly granted into commands
	denyNetwork bool              // run commands with no network (see WithNetworkDenied)
	// networkOpenOK accepts an open network on a tier that cannot deny it (see
	// WithOpenNetworkWhereUndeniable).
	networkOpenOK bool
	readonlyFS    bool           // run commands with a read-only host (see WithReadOnlyFS)
	seccomp       bool           // run commands under a syscall filter (see WithSeccomp)
	egress        *egressConfig  // govern child egress through a netguard proxy (see WithEgress)
	forward       *forwardConfig // forward one host-loopback address into the child's namespace (see WithLoopbackForward)
	// readableDirs are granted on launch and revoked on Close (see WithReadableDir).
	readableDirs []string
	// writableDirs are granted on launch and revoked on Close (see WithWritableDir).
	writableDirs []string
	// scratch holds a confined child's temp files and caches (see scratchDir).
	scratch scratchState
	// unreadable are paths hidden from a confined command (see WithUnreadablePaths).
	unreadable []string
	// confineUnsupported makes a launch refuse because the platform has no kernel
	// confinement (see guardUnsupported).
	confineUnsupported bool
	// hostReadable selects the write-restricted token on Windows (see
	// WithHostReadable).
	hostReadable bool
	// floor lets a refused launch run at the process-jail floor (see
	// WithBestEffortConfinement).
	floor floorPolicy
	// selfExe overrides the re-exec target; empty means /proc/self/exe. Tests set a
	// missing path to force a confinement start failure.
	selfExe string
	// selfTest marks the readiness check's own Local, whose launches must skip the
	// gate that runs the check.
	selfTest bool //nolint:unused // set and read only by the Linux readiness check and gate
	// onExit lets the readiness check see a kill by signal, which ExecResult's exit
	// code cannot carry.
	onExit func(*exec.ExitError)
	// probeOnce guards the one runtime probe of kernel confinement and probeOK caches
	// it (see kernelConfinementEnforceable).
	probeOnce sync.Once
	probeOK   bool
	// grants remembers confirmed Windows directory grants (see ensureGrant).
	grants grantMemo //nolint:unused // used only by the Windows launch paths
}

// LocalOption configures a Local sandbox.
type LocalOption func(*Local)

// WithExecTimeout caps one command's run time. Zero, the default, applies only
// the caller's context.
func WithExecTimeout(d time.Duration) LocalOption {
	return func(l *Local) {
		if d > 0 {
			l.execTimeout = d
		}
	}
}

// ResourceLimits caps a command's committed memory and process count. A zero
// field applies no cap, so the zero value changes nothing.
//
// Windows enforces it through the confined command's job object. Linux and macOS
// do not, because a per-run cgroup needs cgroup-v2 delegation an unprivileged
// agent may lack; a hard cap there needs the container or microVM tier.
type ResourceLimits struct {
	// MemoryMiB caps committed memory in MiB, for the whole job where the platform
	// caps a tree.
	MemoryMiB int
	// MaxProcesses caps concurrently running processes. Zero uses the platform's
	// default cap.
	MaxProcesses int
}

func (r ResourceLimits) set() bool { return r.MemoryMiB > 0 || r.MaxProcesses > 0 }

// WithResourceLimits applies limits; see ResourceLimits for where each is
// enforced.
func WithResourceLimits(r ResourceLimits) LocalOption {
	return func(l *Local) { l.resLimits = r }
}

// WithEnv grants variables beyond the secret-free baseline; it is the only way
// one reaches a command. A granted value overrides a baseline key. Never grant a
// secret here, because the child can read and leak it.
func WithEnv(vars map[string]string) LocalOption {
	return func(l *Local) {
		if len(vars) == 0 {
			return
		}
		if l.granted == nil {
			l.granted = make(map[string]string, len(vars))
		}
		for k, v := range vars {
			l.granted[k] = v
		}
	}
}

// WithNetworkDenied runs commands in a network namespace with no interfaces, so
// they see a down loopback and no routes. Where that cannot be enforced, the
// launch fails. The Windows write-restricted token that [WithDefaultConfinement]
// selects has no network control, so it refuses with [ErrNetworkDeny] unless
// [WithOpenNetworkWhereUndeniable] is set.
func WithNetworkDenied() LocalOption {
	return func(l *Local) { l.denyNetwork = true }
}

// NetworkDenied reports whether the network is denied and the tier enforces it.
// It is false on a tier that cannot deny it (see [NetworkDenySupported]) even
// when asked, so a run record states what was enforced.
func (l *Local) NetworkDenied() bool { return l.networkDenied() }

// WithReadOnlyFS lets commands write only the working tree and a private scratch
// area. It runs them in a mount namespace with every host mount read-only and
// the tree remounted writable. Where that cannot be enforced, the launch fails.
func WithReadOnlyFS() LocalOption {
	return func(l *Local) { l.readonlyFS = true }
}

// WithHostReadable leaves the host readable and still confines writes to the
// workspace. It changes nothing on Linux and macOS, which already read-permit.
// On Windows it replaces the AppContainer with a write-restricted token: a
// contained token cannot query the drive-letter objects that map an NT path to
// a DOS path, so path canonicalization fails, and no file grant fixes that.
//
// Both tiers report ContainmentKernel, so record ConfinementTier as well.
func WithHostReadable() LocalOption {
	return func(l *Local) { l.hostReadable = true }
}

// ConfinementTier names the tier the configuration selects, ignoring any
// fallback. Two tiers with the same Containment can confine reads differently,
// so a run record stores this name too.
func (l *Local) ConfinementTier() string {
	if !l.kernelConfinementEnforceable() {
		return tierFloor
	}
	return l.platformConfinementTier()
}

// ErrReadGrant reports a WithReadableDir grant that failed. The opt-in fallback
// never absorbs it, because that would run the command unconfined. Treat the
// sandbox as unusable.
var ErrReadGrant = errors.New("sandbox: cannot grant read access to a configured directory")

// WithReadableDir grants read and traverse on a directory outside the workspace
// until Close, for an external CLI's auth or config home. It has effect only
// where reads are denied by default (a Windows AppContainer); Linux and macOS
// already permit them. The path is resolved once, like the root.
func WithReadableDir(paths ...string) LocalOption {
	return func(l *Local) {
		for _, p := range paths {
			if p == "" {
				continue
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				abs = resolved
			}
			l.readableDirs = append(l.readableDirs, abs)
		}
	}
}

// ErrWriteGrant reports a WithWritableDir grant that failed. Like ErrReadGrant,
// the fallback never absorbs it, because that would let the child write the
// whole host.
var ErrWriteGrant = errors.New("sandbox: cannot grant write access to a configured directory")

// WithWritableDir grants write on a directory outside the workspace until Close,
// for an external CLI's state such as a refreshed token. It is the only option
// that widens what a child can change, so no default tier sets it. Name a
// directory the run owns and can destroy, because the child can corrupt it. The
// path is resolved once, so a later symlink swap cannot redirect the grant.
func WithWritableDir(paths ...string) LocalOption {
	return func(l *Local) {
		for _, p := range paths {
			if p == "" {
				continue
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				abs = resolved
			}
			l.writableDirs = append(l.writableDirs, abs)
		}
	}
}

// WritableDirs returns a copy of the resolved WithWritableDir grants, the only
// host locations outside the workspace a child could change. The private
// scratch directory is omitted.
func (l *Local) WritableDirs() []string {
	if len(l.writableDirs) == 0 {
		return nil
	}
	out := make([]string, len(l.writableDirs))
	copy(out, l.writableDirs)
	return out
}

// WithSeccomp refuses syscalls used to escalate privilege, escape confinement,
// tamper with the kernel or reach other processes. A refused call fails with a
// permission error instead of killing the command. The filter survives exec.
// Where it cannot be enforced, the launch fails.
func WithSeccomp() LocalOption {
	return func(l *Local) { l.seccomp = true }
}

// WithKernelConfinement is WithNetworkDenied, WithReadOnlyFS and WithSeccomp
// together. Where enforced, Containment reports ContainmentKernel.
func WithKernelConfinement() LocalOption {
	return func(l *Local) {
		l.denyNetwork = true
		l.readonlyFS = true
		l.seccomp = true
	}
}

// WithDefaultConfinement enables the read-only host and the syscall filter. It
// leaves the network open and hides no path, because only the caller knows which
// paths hold credentials; add [WithNetworkDenied] and [WithUnreadablePaths].
//
// A refused launch (on Linux, user namespaces restricted by sysctl, AppArmor or
// a service unit) fails with a [RefusedError], as does a platform with no kernel
// confinement; see [WithBestEffortConfinement]. Windows uses the write-restricted
// token, because the AppContainer denies reads without an ACE for the package
// (see nodejs/node#63590).
func WithDefaultConfinement() LocalOption {
	return func(l *Local) {
		if kernelConfinementSupported() {
			l.readonlyFS = true
			l.seccomp = true
			l.hostReadable = true
			return
		}
		l.confineUnsupported = true
	}
}

// NewLocal resolves dir to an absolute, symlink-free root once, so every check
// compares against the same base.
func NewLocal(dir string, opts ...LocalOption) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	l := &Local{root: abs}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

var _ Sandbox = (*Local)(nil)

// Root returns the absolute working directory the sandbox is confined to.
func (l *Local) Root() string { return l.root }

// Close releases platform state, such as a registered Windows container profile,
// and a running egress proxy.
func (l *Local) Close() error {
	if l.egress != nil {
		l.egress.close()
	}
	if l.forward != nil {
		l.forward.close()
	}
	return l.closePlatform()
}

// Containment is ContainmentNone (a process jail, for trusted code) unless the
// read-only host and syscall filter are both enabled and enforceable on this
// host; then it is ContainmentKernel.
//
// Network egress is excluded because the egress policy governs it per run, so
// the default can still report its tier.
func (l *Local) Containment() Containment {
	if l.kernelConfinementEnforceable() {
		return ContainmentKernel
	}
	return ContainmentNone
}

// kernelConfinementEnforceable probes once and caches. kernelConfinementSupported
// only says the OS has the mechanism; a host can still refuse it (user
// namespaces disabled, a missing re-exec target). The trust gate admits work on
// this answer, so it must be what the host enforces. An unconfigured sandbox is
// not probed.
func (l *Local) kernelConfinementEnforceable() bool {
	if !l.readonlyFS || !l.seccomp || !kernelConfinementSupported() {
		return false
	}
	l.probeOnce.Do(func() { l.probeOK = l.probeConfinement() })
	return l.probeOK
}

// probeConfinement launches a no-op command confined, with the fallback
// bypassed, because only a real launch shows whether the host allows the setup.
func (l *Local) probeConfinement() bool {
	ctx, cancel := context.WithTimeout(withProbe(context.Background()), 10*time.Second)
	defer cancel()
	name, args := shell("exit 0")
	_, err := l.runShell(ctx, name, args, nil, true)
	return err == nil
}

// resolve confines a caller-supplied path to the root and returns the absolute
// path to operate on, or ErrDenied. The nearest existing ancestor is
// symlink-resolved and re-checked so a symlink cannot point the operation outside.
func (l *Local) resolve(p string) (string, error) {
	abs := filepath.Clean(filepath.Join(l.root, p))
	if !l.within(abs) {
		return "", ErrDenied
	}
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !l.within(resolved) {
				return "", ErrDenied
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return abs, nil
}

func (l *Local) within(p string) bool {
	if p == l.root {
		return true
	}
	rel, err := filepath.Rel(l.root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rel keeps absolute host paths out of results.
func (l *Local) rel(abs string) string {
	if r, err := filepath.Rel(l.root, abs); err == nil {
		return filepath.ToSlash(r)
	}
	return abs
}

// Exec runs command in the root through the platform shell. A non-zero exit is
// a result; only a start failure or cancellation is an error.
//
// A refused confinement is returned and the command does not run, unless
// [WithBestEffortConfinement] reruns it at the floor and reports it.
func (l *Local) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	if l.execTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.execTimeout)
		defer cancel()
	}
	// Checked before dispatch because the Windows path skips confine.
	if err := l.guardLaunch(); err != nil {
		return ExecResult{}, err
	}
	// The fallback never weakens egress.
	confined := l.denyNetwork || l.readonlyFS || l.seccomp || l.egress != nil
	res, err := l.execOnce(ctx, cmd, confined)
	// Only a start failure reruns at the floor, and only under the opt-in.
	if l.fallBack(err, confined) {
		return l.execOnce(ctx, cmd, false)
	}
	return res, err
}

func (l *Local) execOnce(ctx context.Context, cmd Command, confined bool) (ExecResult, error) {
	name, args := shell(cmd.Line)
	return l.runShell(ctx, name, args, cmd.Stdin, confined)
}

// runWithExecCmd runs unconfined commands and Linux and macOS confined ones,
// whose confinement fits on an exec.Cmd. Windows confinement does not; see
// runShell.
func (l *Local) runWithExecCmd(ctx context.Context, name string, args []string, stdin []byte, confined bool) (ExecResult, error) {
	//nolint:gosec // running a model-supplied command is the bash tool's purpose; isolation is this sandbox's job, hardened by the stronger tiers
	c := procexec.CommandContext(ctx, name, args...)
	c.Dir = l.root
	// A secret arrives on stdin so it never appears on a command line or in a log.
	if len(stdin) > 0 {
		c.Stdin = bytes.NewReader(stdin)
	}
	// The host environment is never inherited, so the agent's keys stay out.
	c.Env = l.env()
	// The proxy starts before confine, which needs its address to build the
	// allow-only-proxy rule into the same profile or namespace.
	if l.egressActive() {
		release, err := l.startEgress(c)
		if err != nil {
			return ExecResult{}, err
		}
		defer release() // this command runs to completion below, so its proxy ends with it
	}
	if confined {
		if err := l.confine(c); err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: confine: %w", err)
		}
	}
	// The registry answers "children right now", so a hung call must be listed;
	// runCounted counts only a confirmed start.
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := runCounted(c)
	res := ExecResult{Output: buf.String()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitCode()
			if l.onExit != nil {
				l.onExit(exit)
			}
			return res, nil
		}
		return res, fmt.Errorf("sandbox: exec: %w", err)
	}
	return res, nil
}

// ReadFile opens through confinedOpen, so a symlink swapped in after the check
// cannot redirect the read.
func (l *Local) ReadFile(_ context.Context, path string) ([]byte, error) {
	abs, err := l.resolve(path)
	if err != nil {
		return nil, err
	}
	f, err := l.confinedOpen(abs, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// WriteFile creates parent directories and opens through confinedOpen, so a
// swapped symlink cannot redirect the write and a terminal symlink is refused.
func (l *Local) WriteFile(_ context.Context, path string, data []byte) (err error) {
	abs, err := l.resolve(path)
	if err != nil {
		return err
	}
	if err := l.mkdirParents(abs); err != nil {
		return err
	}
	f, err := l.confinedOpen(abs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer closeerr.Keep(&err, f)
	_, err = f.Write(data)
	return err
}

// Glob returns root-relative matches.
func (l *Local) Glob(_ context.Context, pattern string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(l.root, pattern))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if l.within(m) {
			out = append(out, l.rel(m))
		}
	}
	return out, nil
}

// Walk returns the regular-file paths under root, relative to the sandbox root.
func (l *Local) Walk(_ context.Context, root string) ([]string, error) {
	base, err := l.resolve(root)
	if err != nil {
		return nil, err
	}
	var out []string
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error { //nolint:gosec // base is confined to the sandbox root by resolve, symlinks included
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		out = append(out, l.rel(p))
		return nil
	})
	return out, walkErr
}

// env returns the set baseline keys plus grants, which override them, sorted so
// tests see a stable order.
func (l *Local) env() []string {
	vals := make(map[string]string, len(baselineEnvKeys)+len(l.granted))
	for _, k := range baselineEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			vals[k] = v
		}
	}
	for k, v := range l.granted {
		vals[k] = v
	}
	out := make([]string, 0, len(vals))
	for k, v := range vals {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// baselineEnvKeys are the host variables a command inherits: enough for a shell,
// tools, a home and a temp directory, and no credential names. The lists differ
// because cmd.exe will not start without SystemRoot.
var baselineEnvKeys = baselineKeys()

func baselineKeys() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"SystemRoot", "windir", "ComSpec", "PATHEXT", "PATH",
			"TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
			"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
		}
	}
	return []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM"}
}

// shell uses cmd.exe on Windows, the only shell guaranteed present.
func shell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}
