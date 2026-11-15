package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/ionalpha/flynn/internal/procexec"
	"github.com/ionalpha/flynn/procs"
)

// CaptureSpec is a program run to completion without a shell, so no argument can
// be read as shell syntax. Stdout and stderr are combined because a CLI probe
// matches --version or --help text that some CLIs write to stderr. The host
// environment is never inherited, so a secret arrives only through Env.
type CaptureSpec struct {
	// Argv is the program and its arguments, passed verbatim.
	Argv []string
	// Stdin is written to standard input and then closed, keeping a secret off
	// the command line.
	Stdin []byte
	// Env holds KEY=VALUE grants over the deny-by-default baseline; a grant wins
	// for its key.
	Env []string
}

// Capture returns a non-zero exit in ExecResult.ExitCode; an error means the
// command did not run. Confinement follows Exec: a confinement that cannot be
// established fails the launch, and only [WithBestEffortConfinement] retries
// unconfined, never for a governed-egress or network-denied launch.
func (l *Local) Capture(ctx context.Context, spec CaptureSpec) (ExecResult, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return ExecResult{}, errors.New("sandbox: capture: no command")
	}
	if l.execTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.execTimeout)
		defer cancel()
	}
	// Checked here because the Windows path does not run through confine.
	if err := l.guardLaunch(); err != nil {
		return ExecResult{}, err
	}
	confined := l.denyNetwork || l.readonlyFS || l.seccomp || l.egress != nil
	res, err := l.captureRun(ctx, spec, confined)
	if l.fallBack(err, confined) {
		return l.captureRun(ctx, spec, false)
	}
	return res, err
}

// runCounted counts c in the child registry only after a confirmed start, because
// a process that never ran is not a live child. The caller wires c.Stdout and
// c.Stderr first.
func runCounted(c *exec.Cmd) error {
	if err := c.Start(); err != nil {
		return err
	}
	reaped := procs.Started()
	defer reaped()
	return c.Wait()
}

// captureExec serves Linux, macOS and unconfined Windows. Windows confinement is
// an AppContainer, which an exec.Cmd cannot express, so it takes its own path.
func (l *Local) captureExec(ctx context.Context, spec CaptureSpec, confined bool) (ExecResult, error) {
	//nolint:gosec // running the gated external CLI is this primitive's purpose; isolation is the sandbox's job, applied by confine below
	c := procexec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	c.Dir = l.root
	c.Env = l.streamEnv(spec.Env)
	if len(spec.Stdin) > 0 {
		c.Stdin = bytes.NewReader(spec.Stdin)
	}
	// The proxy starts before confine so confine can fold the allow-only-proxy rule
	// into the same enforcement.
	if l.egressActive() {
		release, err := l.startEgress(c)
		if err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: capture: egress: %w", err)
		}
		defer release() // this command runs to completion below, so its proxy ends with it
	}
	if confined {
		if err := l.confine(c); err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: capture: confine: %w", err)
		}
	}
	// Counted while it runs, because a hung one-shot command is what an operator
	// looks for in the registry.
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := runCounted(c)
	res := ExecResult{Output: buf.String()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("sandbox: capture: %w", err)
	}
	return res, nil
}
