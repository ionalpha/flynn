package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"github.com/ionalpha/flynn/fault"
)

// errServeConfineUnsupported is returned when a confined background server is requested
// on a platform that cannot apply its kernel confinement to a backgrounded process (its
// confinement is applied through a blocking launch that yields no backgroundable handle).
// Serve refuses rather than starting the server at the directory-jail floor while the
// sandbox reports the confined tier. It is a governance refusal (Forbidden), like the
// other confinement-unsupported refusals; the always-on best-effort baseline is exempt
// and drops to the floor instead.
var errServeConfineUnsupported = fault.New(fault.Forbidden, "sandbox_serve_confine_unsupported",
	"sandbox: kernel confinement cannot be applied to a background process on this platform; refusing rather than serving the process unconfined")

// ServeSpec describes a long-lived process to run in the background inside the sandbox.
// It is the counterpart of Command for a server that must keep running and be reached
// over a loopback port, rather than a one-shot command whose output is collected and
// returned. The program is run directly from its argv, never through a shell, so a
// model id or weights path in the arguments cannot be reinterpreted as shell syntax.
type ServeSpec struct {
	// Argv is the program and its arguments, executed directly without a shell. Argv[0]
	// is the binary to run; the rest are passed verbatim.
	Argv []string
	// Confine requests the platform's kernel confinement (a read-only host and the
	// syscall filter) for the process where the Local was built to provide it. Network
	// is deliberately left reachable so a loopback server the process binds stays
	// reachable from this host; denying outbound egress while keeping loopback is a
	// separate, finer policy layered on top, not part of starting the server.
	Confine bool
}

// Process is a handle to a background process started by Serve. It is safe for
// concurrent use: Stop, Wait, Done, and Output may be called from any goroutine. The
// process is not tied to the context passed to Serve; it runs until it exits on its
// own or Stop is called, so a server outlives the call that started it.
type Process struct {
	cmd    *exec.Cmd
	tail   *tailBuffer
	done   chan struct{}
	mu     sync.Mutex
	waited bool
	exwait error
}

// tailBufferCap bounds how much of a server's combined output is retained for
// diagnostics. Only the most recent bytes matter when a launch fails, and a server
// can log indefinitely, so the buffer keeps a fixed-size tail rather than growing.
const tailBufferCap = 16 << 10 // 16 KiB

// Serve starts spec.Argv as a background process in the sandbox working directory with
// the deny-by-default environment (the host's environment is never inherited, exactly
// as for Exec), and returns a handle once the process has started. A failure to start
// is an error; a process that starts and later exits is reported through the handle,
// not here. When spec.Confine is set, the process is launched under whatever kernel
// confinement this Local was configured for, so a runtime parsing untrusted weights
// runs read-only to the host and behind the syscall filter where the platform enforces
// them.
//
// Confinement here is expressed on the child process the standard library starts, the
// path Linux and macOS use for a backgrounded server as well as a one-shot command.
// Where a platform cannot carry its confinement onto a backgroundable handle (Windows,
// whose AppContainer is applied through a blocking launch), an explicitly confined Serve
// is refused rather than started at the directory-jail floor under a tier the trust gate
// relied on; only the always-on best-effort baseline is allowed to drop to the floor,
// matching the fallback rule Exec uses.
func (l *Local) Serve(_ context.Context, spec ServeSpec) (*Process, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("sandbox: serve: no command")
	}
	// Governed egress fails closed on a platform without an enforcement leg.
	if err := l.guardEgress(); err != nil {
		return nil, err
	}
	// Egress requires the confined path (its OS-level denial composes into confine)
	// whenever it is configured, regardless of spec.Confine, and like Exec is never
	// weakened by the best-effort fallback.
	confine := l.egress != nil || (spec.Confine && (l.denyNetwork || l.readonlyFS || l.seccomp))
	// On a platform that cannot apply its kernel confinement to a backgrounded process,
	// startProcess would run the server unconfined while the sandbox still reported the
	// confined tier. Rather than that silent drop, refuse the launch. The always-on
	// baseline alone (best-effort, with no egress and no explicit network denial) is
	// allowed to fall to the directory-jail floor, exactly as Exec's fallback permits;
	// an explicit request, governed egress, or a network denial fails closed here.
	if confine && !backgroundConfinementExpressible() {
		if l.egress != nil || l.denyNetwork || !l.confineBestEffort {
			return nil, errServeConfineUnsupported
		}
		return l.startProcess(spec.Argv, false)
	}
	p, err := l.startProcess(spec.Argv, confine)
	// A confined server that could not start under the always-on baseline falls back to
	// the directory-jail floor, exactly as Exec does: the failed attempt never ran, so
	// there is nothing to undo. The same controls Exec refuses to weaken are excluded:
	// egress and an explicit network denial must not drop to an open-network run, and a
	// sandbox reporting the kernel-confined tier must fail closed rather than serve a
	// process unconfined under a guarantee the trust gate already relied on.
	if err != nil && confine && l.confineBestEffort && l.egress == nil && !l.denyNetwork && !l.kernelConfinementEnforceable() {
		return l.startProcess(spec.Argv, false)
	}
	return p, err
}

// startProcess builds and starts the background process once, applying confinement only
// when confined is true, and returns its handle.
func (l *Local) startProcess(argv []string, confined bool) (*Process, error) {
	//nolint:gosec // running the gated runtime binary is this primitive's purpose; isolation is the sandbox's job, applied below
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = l.root
	// Deny-by-default environment: never inherit the host's, so no secret the agent
	// holds reaches the server. It sees only the minimal baseline plus explicit grants.
	c.Env = l.env()
	tail := newTailBuffer(tailBufferCap)
	c.Stdout = tail
	c.Stderr = tail
	// Start the egress proxy and inject its variables before confine, so confine can
	// compose the allow-only-proxy rule into the same enforcement action.
	if l.egressActive() {
		if err := l.startEgress(c); err != nil {
			return nil, fmt.Errorf("sandbox: serve: egress: %w", err)
		}
	}
	if confined {
		if err := l.confine(c); err != nil {
			return nil, fmt.Errorf("sandbox: serve: confine: %w", err)
		}
	}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("sandbox: serve: start: %w", err)
	}
	p := &Process{cmd: c, tail: tail, done: make(chan struct{})}
	go p.reap()
	return p, nil
}

// reap waits for the process to exit exactly once and records the outcome, then closes
// done so every Done/Wait observer is released. Wait() reuses the recorded result so
// the OS-level Wait is never called twice.
func (p *Process) reap() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waited, p.exwait = true, err
	p.mu.Unlock()
	close(p.done)
}

// Done returns a channel closed when the process exits, so a caller can select on it
// alongside a health check or a context.
func (p *Process) Done() <-chan struct{} { return p.done }

// Running reports whether the process has not yet exited.
func (p *Process) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// PID returns the operating-system process id, for a registry record.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Output returns the retained tail of the process's combined stdout and stderr, for
// surfacing why a server failed to come up.
func (p *Process) Output() string { return p.tail.String() }

// Wait blocks until the process exits or ctx is done. It returns the process's exit
// error (nil on a clean exit), or ctx.Err() if the context fires first. The process
// is not killed on a context timeout; the caller decides whether to Stop it.
func (p *Process) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exwait
	}
}

// Stop ends the process and waits for it to exit. It is idempotent: calling it after
// the process has already exited returns nil. A kill is used rather than a graceful
// signal because the server holds no state worth flushing and a prompt, certain stop
// is what a lifecycle command needs.
func (p *Process) Stop() error {
	if !p.Running() {
		return nil
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	// A killed process reports a non-nil wait error; that is the expected outcome of
	// Stop, not a failure, so it is not surfaced.
	return nil
}

// tailBuffer is an io.Writer that retains only the most recent cap bytes written to
// it. It is safe for concurrent writes (the standard library writes stdout and stderr
// from separate goroutines) and reads.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newTailBuffer(capBytes int) *tailBuffer { return &tailBuffer{cap: capBytes} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = t.buf[len(t.buf)-t.cap:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
