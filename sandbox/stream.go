package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ionalpha/flynn/internal/procexec"
	"github.com/ionalpha/flynn/procs"
)

// StreamSpec is a program whose stdout is read live and whose lifetime is bound to
// a context, so a caller can follow an agent CLI's event stream and kill it on a
// halt. Argv runs without a shell, so no argument can be read as shell syntax. The
// host environment is never inherited, so a secret reaches the process only when
// Env grants it by name.
type StreamSpec struct {
	// Argv is the program and its arguments, passed verbatim.
	Argv []string
	// Stdin is written to standard input and then closed. It keeps a secret off
	// the command line and out of logs.
	Stdin []byte
	// Env holds KEY=VALUE grants over the deny-by-default baseline; a grant wins
	// for its key. A single-episode bridge token reaches the child this way.
	Env []string
	// Confine requests kernel confinement. A governed-egress Local confines
	// regardless, so the denial that pins the child to the proxy always holds.
	Confine bool
}

// StreamProcess is a running subprocess killed when the context given to Stream is
// cancelled. Stdout must be drained or the process stalls.
type StreamProcess struct {
	stdout io.Reader
	wait   func() error
}

// Stdout is the live standard output. Standard error is discarded, because an
// agent CLI reports its events on stdout.
func (p *StreamProcess) Stdout() io.Reader { return p.stdout }

// Wait returns the exit outcome and releases the process's resources, so it must
// be called, including after a kill.
func (p *StreamProcess) Wait() error { return p.wait() }

// Stream launches spec.Argv and returns its live stdout; cancelling ctx kills it.
// Confinement follows Exec: a confinement that cannot be established fails the
// launch, and only [WithBestEffortConfinement] retries unconfined, never for a
// governed-egress or network-denied launch.
func (l *Local) Stream(ctx context.Context, spec StreamSpec) (*StreamProcess, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("sandbox: stream: no command")
	}
	// Checked here because the Windows path does not run through confine.
	if err := l.guardLaunch(); err != nil {
		return nil, err
	}
	confined := l.egress != nil || (spec.Confine && (l.denyNetwork || l.readonlyFS || l.seccomp))
	p, err := l.startStream(ctx, spec, confined)
	if l.fallBack(err, confined) {
		return l.startStream(ctx, spec, false)
	}
	return p, err
}

// startStreamExec serves Linux, macOS and unconfined Windows. Windows confinement
// is an AppContainer, which an exec.Cmd cannot express, so it takes its own path.
func (l *Local) startStreamExec(ctx context.Context, spec StreamSpec, confined bool) (*StreamProcess, error) {
	//nolint:gosec // running the gated external CLI is this primitive's purpose; isolation is the sandbox's job, applied by confine below
	c := procexec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	c.Dir = l.root
	c.Env = l.streamEnv(spec.Env)
	if len(spec.Stdin) > 0 {
		c.Stdin = bytes.NewReader(spec.Stdin)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("sandbox: stream: stdout: %w", err)
	}
	c.Stderr = io.Discard
	// The proxy starts before confine so confine can fold the allow-only-proxy rule
	// into the same enforcement. The stdout pipe survives the launcher re-exec.
	release := func() {}
	if l.egressActive() {
		var err error
		if release, err = l.startEgress(c); err != nil {
			return nil, fmt.Errorf("sandbox: stream: egress: %w", err)
		}
	}
	if confined {
		if err := l.confine(c); err != nil {
			release()
			return nil, fmt.Errorf("sandbox: stream: confine: %w", err)
		}
	}
	if err := c.Start(); err != nil {
		release()
		return nil, fmt.Errorf("sandbox: stream: start: %w", err)
	}
	reaped := procs.Started()
	wait := func() error { defer release(); defer reaped(); return c.Wait() }
	return &StreamProcess{stdout: stdout, wait: wait}, nil
}

// streamEnv overlays the KEY=VALUE grants on the deny-by-default baseline.
func (l *Local) streamEnv(grants []string) []string {
	extra := make(map[string]string, len(grants))
	for _, kv := range grants {
		if k, v, ok := strings.Cut(kv, "="); ok {
			extra[k] = v
		}
	}
	return mergeEnv(l.env(), extra)
}
