package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/procexec"
)

// errSessionConfineUnsupported is Forbidden: an extension that cannot be
// contained does not run. Only [WithBestEffortConfinement] drops to the floor.
var errSessionConfineUnsupported = fault.New(fault.Forbidden, "sandbox_session_confine_unsupported",
	"sandbox: kernel confinement cannot be applied to a duplex background process on this platform; refusing rather than running the extension unconfined")

// SessionSpec runs a process with stdin and stdout held open, for MCP JSON-RPC
// over stdio. Anonymous pipes are private to parent and child, where any local
// process could bind or capture a loopback port. No shell runs Argv, and only
// Env grants secrets.
type SessionSpec struct {
	// Argv for an extension is a signature-verified path with fixed arguments that
	// the model cannot influence.
	Argv []string
	// Env overrides the baseline per key. A scoped bridge token goes here, off the
	// command line.
	Env []string
	// Confine applies the Local's kernel confinement. Configured egress confines
	// regardless, so the proxy pin always holds.
	Confine bool
}

// Session is safe for concurrent use. Stop kills the process, so a disabled
// extension leaves no orphan.
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

// Stdin signals end of input when closed; Stop closes it.
func (s *Session) Stdin() io.WriteCloser { return s.stdin }

// Stdout must be drained for the process to make progress.
func (s *Session) Stdout() io.Reader { return s.stdout }

// Stderr returns a bounded tail of standard error.
func (s *Session) Stderr() string { return s.tail.String() }

// Wait memoises the OS wait, so Wait and Stop may both be called. A Stop kill
// returns an error.
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

// Stop is idempotent. It kills, closes stdin and reaps, so no descendant remains.
func (s *Session) Stop() error {
	s.cancel()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	// The kill's wait error is expected.
	_ = s.Wait()
	return nil
}

// Session confines as Serve does: confinement that cannot be established fails
// the launch unless [WithBestEffortConfinement] is set.
func (l *Local) Session(ctx context.Context, spec SessionSpec) (*Session, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("sandbox: session: no command")
	}
	// Governed egress fails closed on a platform without an enforcement leg.
	if err := l.guardLaunch(); err != nil {
		return nil, err
	}
	// Egress always forces confinement and the fallback never weakens it.
	confined := l.egress != nil || (spec.Confine && (l.denyNetwork || l.readonlyFS || l.seccomp))
	// Only the opt-in, without egress or network denial, falls to the floor.
	if confined && !duplexConfinementExpressible() {
		if !l.fallBackUnexpressible(errSessionConfineUnsupported) {
			return nil, errSessionConfineUnsupported
		}
		return l.startSession(ctx, spec, false)
	}
	p, err := l.startSession(ctx, spec, confined)
	// A start error, unlike a non-zero exit, retries at the floor under the opt-in.
	if l.fallBack(err, confined) {
		return l.startSession(ctx, spec, false)
	}
	return p, err
}

// startSessionExec binds the process to an internal context Stop cancels, so a
// kill works whatever the caller's context. Windows uses it only unconfined.
func (l *Local) startSessionExec(ctx context.Context, spec SessionSpec, confined bool) (*Session, error) {
	sctx, cancel := context.WithCancel(ctx)
	//nolint:gosec // running the gated, signature-verified extension binary is this primitive's purpose; isolation is the sandbox's job, applied by confine below
	c := procexec.CommandContext(sctx, spec.Argv[0], spec.Argv[1:]...)
	c.Dir = l.root
	// No agent secret leaks into the extension through the environment.
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

	// The proxy starts before confine so confine can add the proxy-only rule. The
	// pipe fds survive the launcher re-exec.
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
