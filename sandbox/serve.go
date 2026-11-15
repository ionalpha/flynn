package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/procexec"
	"github.com/ionalpha/flynn/procs"
)

// errServeConfineUnsupported is Forbidden: the platform confines only through a
// blocking launch, and starting at the jail floor would misreport the tier. Only
// [WithBestEffortConfinement] drops to the floor.
var errServeConfineUnsupported = fault.New(fault.Forbidden, "sandbox_serve_confine_unsupported",
	"sandbox: kernel confinement cannot be applied to a background process on this platform; refusing rather than serving the process unconfined")

// ServeSpec describes a server reached over loopback. Argv never passes through a
// shell, so a model id or weights path cannot become shell syntax.
type ServeSpec struct {
	// Argv[0] is the binary; the rest pass verbatim.
	Argv []string
	// Confine applies the Local's read-only host and syscall filter. The network
	// stays reachable so the loopback server can be reached; egress is a separate
	// policy.
	Confine bool
}

// Process is safe for concurrent use. It ignores Serve's context and runs until
// it exits or Stop is called.
type Process struct {
	cmd  *exec.Cmd
	tail *tailBuffer
	done chan struct{}
	// release frees per-process launch state, such as an egress proxy, after reap.
	release func()
	// reaped runs only from reap, the one goroutine that waits on the command.
	reaped func()
	mu     sync.Mutex
	waited bool
	exwait error
}

// tailBufferCap bounds retained output because a server can log forever and a
// failed launch needs only the tail.
const tailBufferCap = 16 << 10 // 16 KiB

// Serve starts spec.Argv in the working directory without the host environment.
// Only a failure to start is an error; a later exit shows on the handle.
//
// Windows applies AppContainer through a blocking launch, so a confined Serve is
// refused there unless [WithBestEffortConfinement] allows the floor.
func (l *Local) Serve(_ context.Context, spec ServeSpec) (*Process, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("sandbox: serve: no command")
	}
	// Governed egress fails closed on a platform without an enforcement leg.
	if err := l.guardLaunch(); err != nil {
		return nil, err
	}
	// Egress always forces confinement and the fallback never weakens it.
	confine := l.egress != nil || (spec.Confine && (l.denyNetwork || l.readonlyFS || l.seccomp))
	// startProcess would run unconfined under the confined tier, so refuse. Only the
	// opt-in, without egress or network denial, falls to the floor, and says so.
	if confine && !backgroundConfinementExpressible() {
		if !l.fallBackUnexpressible(errServeConfineUnsupported) {
			return nil, errServeConfineUnsupported
		}
		return l.startProcess(spec.Argv, false)
	}
	p, err := l.startProcess(spec.Argv, confine)
	// A start error, unlike a non-zero exit, retries at the floor under the opt-in.
	if l.fallBack(err, confine) {
		return l.startProcess(spec.Argv, false)
	}
	return p, err
}

func (l *Local) startProcess(argv []string, confined bool) (*Process, error) {
	//nolint:gosec // running the gated runtime binary is this primitive's purpose; isolation is the sandbox's job, applied below
	c := procexec.Command(argv[0], argv[1:]...)
	c.Dir = l.root
	// No agent secret reaches the server through the environment.
	c.Env = l.env()
	tail := newTailBuffer(tailBufferCap)
	c.Stdout = tail
	c.Stderr = tail
	// The proxy starts before confine so confine can add the proxy-only rule.
	release := func() {}
	if l.egressActive() {
		var err error
		if release, err = l.startEgress(c); err != nil {
			return nil, fmt.Errorf("sandbox: serve: egress: %w", err)
		}
	}
	if confined {
		if err := l.confine(c); err != nil {
			release()
			return nil, fmt.Errorf("sandbox: serve: confine: %w", err)
		}
	}
	if err := c.Start(); err != nil {
		release()
		return nil, fmt.Errorf("sandbox: serve: start: %w", err)
	}
	p := &Process{cmd: c, tail: tail, done: make(chan struct{}), release: release, reaped: procs.Started()}
	go p.reap()
	return p, nil
}

// reap is the only caller of the OS Wait, which must not run twice.
func (p *Process) reap() {
	err := p.cmd.Wait()
	p.reaped()
	p.release()
	p.mu.Lock()
	p.waited, p.exwait = true, err
	p.mu.Unlock()
	close(p.done)
}

// Done is closed when the process exits.
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

// Output returns the tail of combined stdout and stderr.
func (p *Process) Output() string { return p.tail.String() }

// Wait does not kill the process when ctx ends; the caller decides on Stop.
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

// Stop is idempotent. It kills because the server has no state to flush.
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
	// The kill's wait error is expected.
	return nil
}

// tailBuffer keeps the last cap bytes. It is safe for concurrent use because
// stdout and stderr are written from separate goroutines.
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
