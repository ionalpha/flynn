package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/ionalpha/flynn/procs"
)

// StreamSpec is a program to launch under the sandbox with its stdout delivered as a
// live stream and its lifetime bound to a context, for a caller that reads the
// process's output as it is produced and stops it on a halt. It is the streaming
// counterpart of Command (one-shot, combined output collected and returned) and
// ServeSpec (backgrounded, only a buffered tail kept): the external-agent episode host
// reads an agent CLI's newline-delimited event stream and kills the CLI when the run
// halts.
//
// The program is run directly from its Argv, never through a shell, so a model id, a
// path, or any other value in the arguments cannot be reinterpreted as shell syntax.
// The deny-by-default environment applies exactly as it does to Exec and Serve: the
// host environment is never inherited, so no secret the agent holds reaches the process
// unless Env grants it by name.
type StreamSpec struct {
	// Argv is the program and its arguments, executed directly without a shell. Argv[0]
	// is the binary to run; the rest are passed verbatim.
	Argv []string
	// Stdin, when non-empty, is written to the process's standard input and the stream is
	// then closed. Like Command.Stdin it keeps a secret off the command line: the value
	// travels only on the pipe, never in argv or a log.
	Stdin []byte
	// Env grants additional KEY=VALUE variables on top of the sandbox's deny-by-default
	// baseline, the same brokered path WithEnv feeds a one-shot command. A granted value
	// overrides the baseline value for the same key. It is how a scoped, single-episode
	// bridge token reaches the child without ever touching the command line.
	Env []string
	// Confine requests the platform's kernel confinement (a read-only host and the syscall
	// filter, plus the network isolation the Local was configured for). It composes with a
	// governed-egress Local: when egress is configured the launch is confined regardless of
	// this flag, so the OS-level denial that pins the child to the proxy is always in force.
	Confine bool
}

// StreamProcess is a running confined subprocess: its stdout as a live stream plus a
// wait handle. Its lifetime is bound to the context passed to Stream, so cancelling
// that context (a halt or a shutdown) kills the process and Wait then returns. Stdout
// must be drained for the process to make progress; Wait blocks until the process ends
// and returns its outcome (nil on a clean exit, a non-nil error on a non-zero exit or a
// context-driven kill). A caller that has finished reading before the process exits
// calls Wait to release its resources.
type StreamProcess struct {
	stdout io.Reader
	wait   func() error
}

// Stdout is the process's live standard output. Standard error is not part of the
// stream (an agent CLI reports its events on stdout); it is discarded.
func (p *StreamProcess) Stdout() io.Reader { return p.stdout }

// Wait blocks until the process exits and returns its outcome. It also releases the
// process's resources, so it must be called once the caller is done with the process,
// including after a context-driven kill.
func (p *StreamProcess) Wait() error { return p.wait() }

// Stream launches spec.Argv as a confined subprocess and returns a handle to its live
// stdout, bound to ctx: cancelling ctx kills the process. It is the streaming launch
// path, alongside Exec (one-shot) and Serve (backgrounded). Confinement follows the
// same rules as Exec: a governed-egress Local always takes the confined path so its
// OS-level egress denial holds; an explicit Confine request is honored where the
// platform can enforce it and, unless this Local is the always-on best-effort baseline,
// a confinement that cannot be established fails the launch rather than dropping to an
// unconfined run (refuse-rather-than-downgrade). The best-effort fallback, when it
// applies, never weakens a governed-egress or network-denied launch.
func (l *Local) Stream(ctx context.Context, spec StreamSpec) (*StreamProcess, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("sandbox: stream: no command")
	}
	// Governed egress fails closed on a platform without an enforcement leg, before any
	// launch (the Windows path does not run through confine).
	if err := l.guardEgress(); err != nil {
		return nil, err
	}
	// Egress requires the confined path (its OS-level denial composes into confine)
	// whenever it is configured, regardless of spec.Confine, and like Exec is never
	// weakened by the best-effort fallback.
	confined := l.egress != nil || (spec.Confine && (l.denyNetwork || l.readonlyFS || l.seccomp))
	p, err := l.startStream(ctx, spec, confined)
	// A confined launch that could not start (an error, not a non-zero exit) under the
	// always-on baseline falls back to the floor, exactly as Exec does; the failed attempt
	// never ran, so there is nothing to undo. The controls Exec refuses to weaken are
	// excluded here too: a governed-egress or network-denied launch, and a sandbox that
	// reports the kernel-confined tier the trust gate already relied on, fail closed rather
	// than dropping to an unconfined run.
	if err != nil && confined && l.confineBestEffort && l.egress == nil && !l.denyNetwork && !l.kernelConfinementEnforceable() && !errors.Is(err, ErrReadGrant) {
		return l.startStream(ctx, spec, false)
	}
	return p, err
}

// startStreamExec launches a streamed subprocess through the standard library, applying
// this platform's exec.Cmd-expressible confinement via confine when confined is true. It
// mirrors runWithExecCmd's confined launch but wires stdout to a live pipe and returns
// before the process exits. The process is bound to ctx through exec.CommandContext, so a
// cancellation kills it; stderr is discarded because an agent CLI reports its events on
// stdout and a confined child's diagnostics are not part of the stream. It is the launch
// path for every platform's exec.Cmd confinement (Linux and macOS) and for Windows's
// unconfined case; Windows confinement is an AppContainer, which cannot be expressed on an
// exec.Cmd, so it takes its own path.
func (l *Local) startStreamExec(ctx context.Context, spec StreamSpec, confined bool) (*StreamProcess, error) {
	//nolint:gosec // running the gated external CLI is this primitive's purpose; isolation is the sandbox's job, applied by confine below
	c := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	c.Dir = l.root
	// Deny-by-default environment plus the caller's explicit grants; the host's is never
	// inherited, so no agent secret leaks in and a bridge token reaches the child only
	// because it was granted by name.
	c.Env = l.streamEnv(spec.Env)
	if len(spec.Stdin) > 0 {
		c.Stdin = bytes.NewReader(spec.Stdin)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("sandbox: stream: stdout: %w", err)
	}
	c.Stderr = io.Discard
	// Start the egress proxy and inject its variables before confine, so confine can
	// compose the allow-only-proxy rule into the same enforcement action; then apply the
	// platform confinement. The stdout pipe fd is inherited across the confinement re-exec,
	// so it stays connected to the real command the launcher execs.
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
	// The proxy serving this child alone is released when the child is waited on, which is
	// the moment the caller learns it has exited, and the same moment the registry stops
	// counting it as live.
	reaped := procs.Started()
	wait := func() error { defer release(); defer reaped(); return c.Wait() }
	return &StreamProcess{stdout: stdout, wait: wait}, nil
}

// streamEnv builds the environment for a streamed process: the deny-by-default baseline
// (env) plus the caller's explicit KEY=VALUE grants, overlaid so a grant overrides a
// baseline value for the same key. The host environment is never inherited, so a secret
// reaches the child only when it is granted by name.
func (l *Local) streamEnv(grants []string) []string {
	extra := make(map[string]string, len(grants))
	for _, kv := range grants {
		if k, v, ok := strings.Cut(kv, "="); ok {
			extra[k] = v
		}
	}
	return mergeEnv(l.env(), extra)
}
