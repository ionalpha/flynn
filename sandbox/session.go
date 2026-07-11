package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/ionalpha/flynn/fault"
)

// errSessionConfineUnsupported is returned when a confined duplex session is requested on
// a platform that cannot apply its kernel confinement to a backgrounded, still-writable
// process. Like Serve, Session refuses rather than starting the process at the
// directory-jail floor while the sandbox reports the confined tier: an out-of-process
// extension that cannot be contained must not run at all. It is a governance refusal
// (Forbidden); only the always-on best-effort baseline drops to the floor instead.
var errSessionConfineUnsupported = fault.New(fault.Forbidden, "sandbox_session_confine_unsupported",
	"sandbox: kernel confinement cannot be applied to a duplex background process on this platform; refusing rather than running the extension unconfined")

// SessionSpec describes a long-lived process to run inside the sandbox with BOTH its
// standard input and output held open as live pipes, for a caller that carries on a
// request/response conversation with the process over its own stdio. It is the duplex
// counterpart of Stream (stdin written once then closed, stdout streamed) and Serve
// (backgrounded, reached over a loopback port): the out-of-process extension host speaks
// MCP JSON-RPC to a subprocess and must both write requests to it and read replies from
// it for the life of the process.
//
// The channel is deliberately the process's own anonymous stdio pipes and nothing else:
// a pipe fd is private to this parent and its child, where a loopback port any co-located
// process could bind or capture is not, so an extension cannot be snooped or injected on
// its channel. The program is run directly from Argv, never through a shell, so nothing
// in the arguments can be reinterpreted as shell syntax, and the deny-by-default
// environment applies exactly as for Exec, Stream, and Serve: the host environment is
// never inherited, so no secret the agent holds reaches the process unless Env grants it
// by name.
type SessionSpec struct {
	// Argv is the program and its arguments, executed directly without a shell. Argv[0] is
	// the binary to run; the rest are passed verbatim. For an extension it is a fixed,
	// signature-verified path with fixed arguments, never model-influenced.
	Argv []string
	// Env grants additional KEY=VALUE variables on top of the deny-by-default baseline. A
	// granted value overrides the baseline value for the same key. An extension is launched
	// with a minimal env and no secrets; a scoped bridge token, when one is needed, reaches
	// the child here rather than on the command line.
	Env []string
	// Confine requests the platform's kernel confinement (a read-only host and the syscall
	// filter, plus the network isolation the Local was configured for). It composes with a
	// governed-egress Local: when egress is configured the launch is confined regardless of
	// this flag, so the OS-level denial that pins the child to the proxy is always in force.
	Confine bool
}

// Session is a running confined subprocess whose stdin and stdout are live pipes and
// whose stderr is retained as a bounded tail for diagnostics. It is safe for concurrent
// use: Stdin, Stdout, Stderr, Wait, and Stop may be called from any goroutine. The
// process runs until it exits on its own or Stop is called; Stop kills it and releases
// its resources, so an extension leaves no orphan when it is disabled or the host shuts
// down.
type Session struct {
	stdin  io.WriteCloser
	stdout io.Reader
	tail   *tailBuffer
	cancel context.CancelFunc
	wait   func() error

	mu      sync.Mutex
	waited  bool
	waitErr error
}

// Stdin is the process's standard input, held open for the caller to write requests to.
// Closing it signals end-of-input to the process; Stop closes it as part of teardown.
func (s *Session) Stdin() io.WriteCloser { return s.stdin }

// Stdout is the process's live standard output, for the caller to read replies from. It
// must be drained for the process to make progress.
func (s *Session) Stdout() io.Reader { return s.stdout }

// Stderr returns the retained tail of the process's standard error, for surfacing why an
// extension misbehaved or failed to come up. It is bounded, so a chatty process cannot
// grow it without limit.
func (s *Session) Stderr() string { return s.tail.String() }

// Wait blocks until the process exits and returns its outcome (nil on a clean exit, a
// non-nil error on a non-zero exit or a Stop-driven kill). It is idempotent: the OS-level
// wait runs once and its result is memoised, so Wait and Stop may both be called.
func (s *Session) Wait() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waited {
		return s.waitErr
	}
	s.waitErr = s.wait()
	s.waited = true
	return s.waitErr
}

// Stop ends the process and waits for it to exit. It is idempotent and safe to call more
// than once. It cancels the launch context (killing the process), closes the stdin pipe,
// and reaps the process so no descendant is left behind.
func (s *Session) Stop() error {
	s.cancel()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	// A killed process reports a non-nil wait error; that is the expected outcome of Stop,
	// not a failure, so Wait's result is consumed but not surfaced here.
	_ = s.Wait()
	return nil
}

// Session launches spec.Argv as a confined duplex subprocess and returns a handle to its
// stdin and stdout pipes, its stderr tail, and its lifecycle. Confinement follows the
// same rules as Exec, Stream, and Serve: a governed-egress Local always takes the confined
// path so its OS-level egress denial holds; an explicit Confine request is honoured where
// the platform can enforce it and, unless this Local is the always-on best-effort
// baseline, a confinement that cannot be established fails the launch rather than dropping
// to an unconfined run (refuse-rather-than-downgrade). Where a platform cannot carry its
// confinement onto a still-writable background process, a confined Session is refused
// rather than run unconfined, exactly as Serve refuses a confined background server.
func (l *Local) Session(ctx context.Context, spec SessionSpec) (*Session, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("sandbox: session: no command")
	}
	// Governed egress fails closed on a platform without an enforcement leg, before any
	// launch.
	if err := l.guardEgress(); err != nil {
		return nil, err
	}
	// Egress requires the confined path (its OS-level denial composes into confine)
	// whenever it is configured, regardless of spec.Confine, and like Exec is never
	// weakened by the best-effort fallback.
	confined := l.egress != nil || (spec.Confine && (l.denyNetwork || l.readonlyFS || l.seccomp))
	// On a platform that cannot apply its kernel confinement to a duplex background
	// process, refuse rather than silently dropping to the directory-jail floor under a
	// tier the trust gate relied on. Only the always-on best-effort baseline (no egress,
	// no explicit network denial) is allowed to fall to the floor, as Serve permits.
	if confined && !duplexConfinementExpressible() {
		if l.egress != nil || l.denyNetwork || !l.confineBestEffort {
			return nil, errSessionConfineUnsupported
		}
		return l.startSession(ctx, spec, false)
	}
	p, err := l.startSession(ctx, spec, confined)
	// A confined launch that could not start (an error, not a non-zero exit) under the
	// always-on baseline falls back to the floor, exactly as Exec and Stream do; the
	// failed attempt never ran, so there is nothing to undo. The controls Exec refuses to
	// weaken are excluded here too: a governed-egress or network-denied launch, and a
	// sandbox reporting the kernel-confined tier the trust gate already relied on, fail
	// closed rather than dropping to an unconfined run.
	if err != nil && confined && l.confineBestEffort && l.egress == nil && !l.denyNetwork && !l.kernelConfinementEnforceable() && !errors.Is(err, ErrReadGrant) {
		return l.startSession(ctx, spec, false)
	}
	return p, err
}

// startSessionExec launches a duplex subprocess through the standard library, applying
// this platform's exec.Cmd-expressible confinement via confine when confined is true. It
// mirrors startStreamExec but wires stdin to a live pipe (rather than a one-shot reader)
// alongside the live stdout pipe, and keeps stderr as a bounded tail rather than
// discarding it. The process is bound to an internal context cancelled by Stop, so a kill
// is always available regardless of the caller's context. It is the launch path for every
// platform's exec.Cmd confinement (Linux and macOS) and for Windows's unconfined case.
func (l *Local) startSessionExec(ctx context.Context, spec SessionSpec, confined bool) (*Session, error) {
	sctx, cancel := context.WithCancel(ctx)
	//nolint:gosec // running the gated, signature-verified extension binary is this primitive's purpose; isolation is the sandbox's job, applied by confine below
	c := exec.CommandContext(sctx, spec.Argv[0], spec.Argv[1:]...)
	c.Dir = l.root
	// Deny-by-default environment plus the caller's explicit grants; the host's is never
	// inherited, so no agent secret leaks into an untrusted extension.
	c.Env = l.streamEnv(spec.Env)

	stdin, err := c.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("sandbox: session: stdin: %w", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("sandbox: session: stdout: %w", err)
	}
	tail := newTailBuffer(tailBufferCap)
	c.Stderr = tail

	// Start the egress proxy and inject its variables before confine, so confine can
	// compose the allow-only-proxy rule into the same enforcement action; then apply the
	// platform confinement. The pipe fds are inherited across the confinement re-exec, so
	// they stay connected to the real command the launcher execs.
	release := func() {}
	if l.egressActive() {
		if release, err = l.startEgress(c); err != nil {
			cancel()
			return nil, fmt.Errorf("sandbox: session: egress: %w", err)
		}
	}
	if confined {
		if err := l.confine(c); err != nil {
			release()
			cancel()
			return nil, fmt.Errorf("sandbox: session: confine: %w", err)
		}
	}
	if err := c.Start(); err != nil {
		release()
		cancel()
		return nil, fmt.Errorf("sandbox: session: start: %w", err)
	}
	wait := func() error { defer release(); return c.Wait() }
	return &Session{stdin: stdin, stdout: stdout, tail: tail, cancel: cancel, wait: wait}, nil
}
