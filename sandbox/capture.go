package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// CaptureSpec is a program to run to completion under the sandbox with its combined
// standard output and standard error collected and returned. It is the argv counterpart
// of Command: Command runs a shell line (sh -c / cmd /c), while Capture runs the program
// directly from its Argv, never through a shell, so a path or flag cannot be reinterpreted
// as shell syntax. It is for a caller that must run a program directly and read all of its
// output, the external-agent detection probe being the first: it runs a CLI's
// --version/--help/status and matches on the text, some of which a CLI writes to stderr.
//
// The deny-by-default environment applies exactly as it does to Exec, Serve, and Stream:
// the host environment is never inherited, so no secret the agent holds reaches the
// process unless Env grants it by name.
type CaptureSpec struct {
	// Argv is the program and its arguments, executed directly without a shell. Argv[0]
	// is the binary to run; the rest are passed verbatim.
	Argv []string
	// Stdin, when non-empty, is written to the process's standard input and the stream is
	// then closed, the same off-the-command-line path Command.Stdin uses for a secret.
	Stdin []byte
	// Env grants additional KEY=VALUE variables on top of the sandbox's deny-by-default
	// baseline, the same brokered path WithEnv and StreamSpec.Env feed. A granted value
	// overrides the baseline value for the same key.
	Env []string
}

// Capture runs spec.Argv to completion as a confined subprocess and returns its combined
// output and exit code. It is the run-to-completion argv launch path, alongside Exec (a
// shell line), Serve (backgrounded), and Stream (streaming). A non-zero exit is a normal
// result carried in ExecResult.ExitCode, not an error; an error means the command could
// not be run at all.
//
// Confinement follows the same rules as Exec: whatever isolation the Local was configured
// for is applied (a governed-egress Local always takes the confined path so its OS-level
// egress denial holds), and, unless this Local is the always-on best-effort baseline, a
// confinement that cannot be established fails the launch rather than dropping to an
// unconfined run. The best-effort fallback, when it applies, never weakens a
// governed-egress or network-denied launch.
func (l *Local) Capture(ctx context.Context, spec CaptureSpec) (ExecResult, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return ExecResult{}, errors.New("sandbox: capture: no command")
	}
	if l.execTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.execTimeout)
		defer cancel()
	}
	// Governed egress fails closed on a platform without an enforcement leg, before any
	// launch (the Windows path does not run through confine).
	if err := l.guardEgress(); err != nil {
		return ExecResult{}, err
	}
	// Egress requires the confined path (its OS-level denial composes into confine), and
	// like Exec any configured isolation confines the run. A network control is never
	// weakened by the best-effort fallback below.
	confined := l.denyNetwork || l.readonlyFS || l.seccomp || l.egress != nil
	res, err := l.captureRun(ctx, spec, confined)
	// A confined launch that could not start (an error, not a non-zero exit) under the
	// always-on baseline falls back to the floor, exactly as Exec does; the failed attempt
	// never ran, so there is nothing to undo. The controls Exec refuses to weaken are
	// excluded here too: a governed-egress or network-denied launch, and a sandbox that
	// reports the kernel-confined tier the trust gate already relied on, fail closed rather
	// than dropping to an unconfined run.
	if err != nil && confined && l.confineBestEffort && l.egress == nil && !l.denyNetwork && !l.kernelConfinementEnforceable() && !errors.Is(err, ErrReadGrant) {
		return l.captureRun(ctx, spec, false)
	}
	return res, err
}

// captureExec runs spec.Argv to completion through the standard library, applying this
// platform's exec.Cmd-expressible confinement via confine when confined is true. It
// mirrors runWithExecCmd's confined launch but takes an argv directly (no shell) and the
// caller's Env grants. It is the launch path for every platform's exec.Cmd confinement
// (Linux and macOS) and for Windows's unconfined case; Windows confinement is an
// AppContainer, which cannot be expressed on an exec.Cmd, so it takes its own path.
func (l *Local) captureExec(ctx context.Context, spec CaptureSpec, confined bool) (ExecResult, error) {
	//nolint:gosec // running the gated external CLI is this primitive's purpose; isolation is the sandbox's job, applied by confine below
	c := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	c.Dir = l.root
	// Deny-by-default environment plus the caller's explicit grants; the host's is never
	// inherited, so no agent secret leaks in.
	c.Env = l.streamEnv(spec.Env)
	if len(spec.Stdin) > 0 {
		c.Stdin = bytes.NewReader(spec.Stdin)
	}
	// Start the egress proxy and inject its variables before confine, so confine can
	// compose the allow-only-proxy rule into the same enforcement action; then apply the
	// platform confinement.
	if l.egressActive() {
		if err := l.startEgress(c); err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: capture: egress: %w", err)
		}
	}
	if confined {
		if err := l.confine(c); err != nil {
			return ExecResult{}, fmt.Errorf("sandbox: capture: confine: %w", err)
		}
	}
	out, err := c.CombinedOutput()
	res := ExecResult{Output: string(out)}
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
